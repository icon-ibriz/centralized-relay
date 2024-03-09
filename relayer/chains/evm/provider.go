package evm

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/pkg/errors"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	coreTypes "github.com/ethereum/go-ethereum/core/types"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	bridgeContract "github.com/icon-project/centralized-relay/relayer/chains/evm/abi"
	"github.com/icon-project/centralized-relay/relayer/chains/evm/types"
	"github.com/icon-project/centralized-relay/relayer/events"
	"github.com/icon-project/centralized-relay/relayer/kms"
	"github.com/icon-project/centralized-relay/relayer/provider"
	providerTypes "github.com/icon-project/centralized-relay/relayer/types"

	"go.uber.org/zap"
)

var _ provider.Config = (*Config)(nil)

var (
	MethodRecvMessage = "recvMessage"
	MethodExecuteCall = "executeCall"
)

type Config struct {
	ChainName      string                          `json:"-" yaml:"-"`
	RPCUrl         string                          `json:"rpc-url" yaml:"rpc-url"`
	VerifierRPCUrl string                          `json:"verifier-rpc-url" yaml:"verifier-rpc-url"`
	StartHeight    uint64                          `json:"start-height" yaml:"start-height"`
	Address        string                          `json:"address" yaml:"address"`
	GasPrice       uint64                          `json:"gas-price" yaml:"gas-price"`
	GasMin         uint64                          `json:"gas-min" yaml:"gas-min"`
	GasLimit       uint64                          `json:"gas-limit" yaml:"gas-limit"`
	Contracts      providerTypes.ContractConfigMap `json:"contracts" yaml:"contracts"`
	Concurrency    uint64                          `json:"concurrency" yaml:"concurrency"`
	FinalityBlock  uint64                          `json:"finality-block" yaml:"finality-block"`
	BlockInterval  time.Duration                   `json:"block-interval" yaml:"block-interval"`
	NID            string                          `json:"nid" yaml:"nid"`
	HomeDir        string                          `json:"-" yaml:"-"`
}

type Provider struct {
	client       IClient
	verifier     IClient
	log          *zap.Logger
	cfg          *Config
	StartHeight  uint64
	blockReq     ethereum.FilterQuery
	wallet       *keystore.Key
	kms          kms.KMS
	contracts    map[string]providerTypes.EventMap
	NonceTracker types.NonceTrackerI
}

func (p *Config) NewProvider(ctx context.Context, log *zap.Logger, homepath string, debug bool, chainName string) (provider.ChainProvider, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	p.HomeDir = homepath
	p.ChainName = chainName

	connectionContract := common.HexToAddress(p.Contracts[providerTypes.ConnectionContract])
	xcallContract := common.HexToAddress(p.Contracts[providerTypes.XcallContract])

	client, err := newClient(ctx, connectionContract, xcallContract, p.RPCUrl, log)
	if err != nil {
		return nil, fmt.Errorf("error occured when creating client: %v", err)
	}

	var verifierClient IClient

	if p.VerifierRPCUrl != "" {
		var err error
		verifierClient, err = newClient(ctx, connectionContract, xcallContract, p.RPCUrl, log)
		if err != nil {
			return nil, err
		}
	} else {
		verifierClient = client // default to same client
	}

	// setting default finality block
	if p.FinalityBlock == 0 {
		p.FinalityBlock = uint64(DefaultFinalityBlock)
	}

	return &Provider{
		cfg:          p,
		log:          log.With(zap.Stringp("nid", &p.NID), zap.Stringp("name", &p.ChainName)),
		client:       client,
		blockReq:     p.GetMonitorEventFilters(),
		verifier:     verifierClient,
		contracts:    p.eventMap(),
		NonceTracker: types.NewNonceTracker(),
	}, nil
}

func (p *Provider) NID() string {
	return p.cfg.NID
}

func (p *Config) Validate() error {
	if err := p.Contracts.Validate(); err != nil {
		return fmt.Errorf("contracts are not valid: %s", err)
	}
	return nil
}

func (p *Config) SetWallet(addr string) {
	p.Address = addr
}

func (p *Config) GetWallet() string {
	return p.Address
}

func (p *Provider) Init(ctx context.Context, homePath string, kms kms.KMS) error {
	p.kms = kms
	return nil
}

func (p *Provider) Type() string {
	return "evm"
}

func (p *Provider) Config() provider.Config {
	return p.cfg
}

func (p *Provider) Name() string {
	return p.cfg.ChainName
}

func (p *Provider) Wallet() (*keystore.Key, error) {
	if p.wallet == nil {
		if err := p.RestoreKeystore(context.Background()); err != nil {
			return nil, err
		}
		if p.NonceTracker.Get(p.wallet.Address) == nil {
			nonce, err := p.client.NonceAt(context.Background(), p.wallet.Address, nil)
			if err != nil {
				return nil, err
			}
			p.NonceTracker.Set(p.wallet.Address, nonce)
		}
	}
	return p.wallet, nil
}

func (p *Provider) FinalityBlock(ctx context.Context) uint64 {
	return p.cfg.FinalityBlock
}

func (p *Provider) WaitForResults(ctx context.Context, txHash common.Hash) (*coreTypes.Receipt, error) {
	ticker := time.NewTicker(DefaultGetTransactionResultPollingInterval * 2)
	var retryCounter uint8
	for {
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if retryCounter >= providerTypes.MaxTxRetry {
				return nil, fmt.Errorf("max retry reached for tx %s", txHash.String())
			}
			retryCounter++
			txr, err := p.client.TransactionReceipt(ctx, txHash)
			if err == ethereum.NotFound {
				continue
			}
			return txr, err
		}
	}
}

func (r *Provider) transferBalance(senderKey, recepientAddress string, amount *big.Int) (txnHash common.Hash, err error) {
	from, err := crypto.HexToECDSA(senderKey)
	if err != nil {
		return common.Hash{}, err
	}

	fromAddress := crypto.PubkeyToAddress(from.PublicKey)

	nonce, err := r.client.NonceAt(context.TODO(), fromAddress, nil)
	if err != nil {
		err = errors.Wrap(err, "PendingNonceAt ")
		return common.Hash{}, err
	}
	gasPrice, err := r.client.SuggestGasPrice(context.Background())
	if err != nil {
		err = errors.Wrap(err, "SuggestGasPrice ")
		return common.Hash{}, err
	}
	chainID := r.client.GetChainID()
	tx := ethTypes.NewTransaction(nonce.Uint64(), common.HexToAddress(recepientAddress), amount, 30000000, gasPrice, []byte{})
	signedTx, err := ethTypes.SignTx(tx, ethTypes.NewEIP155Signer(chainID), from)
	if err != nil {
		err = errors.Wrap(err, "SignTx ")
		return common.Hash{}, err
	}

	if err = r.client.SendTransaction(context.Background(), signedTx); err != nil {
		err = errors.Wrap(err, "SendTransaction ")
		return
	}
	txnHash = signedTx.Hash()
	return
}

func (p *Provider) GetTransationOpts(ctx context.Context) (*bind.TransactOpts, error) {
	newTransactOpts := func(w *keystore.Key) (*bind.TransactOpts, error) {
		txo, err := bind.NewKeyedTransactorWithChainID(w.PrivateKey, p.client.GetChainID())
		if err != nil {
			return nil, err
		}
		return txo, nil
	}

	wallet, err := p.Wallet()
	if err != nil {
		return nil, err
	}

	txOpts, err := newTransactOpts(p.wallet)
	if err != nil {
		return nil, err
	}
	txOpts.Nonce = p.NonceTracker.Get(wallet.Address)
	txOpts.Context = ctx
	gasPrice, err := p.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get gas price: %w", err)
	}
	txOpts.GasPrice = gasPrice
	return txOpts, nil
}

// SetAdmin sets the admin address of the bridge contract
func (p *Provider) SetAdmin(ctx context.Context, admin string) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	tx, err := p.SendTransaction(ctx, opts, &providerTypes.Message{EventType: events.SetAdmin, Dst: admin}, providerTypes.MaxTxRetry)
	receipt, err := p.WaitForResults(ctx, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return fmt.Errorf("failed to set admin: %s", err)
	}
	return nil
}

// RevertMessage
func (p *Provider) RevertMessage(ctx context.Context, sn *big.Int) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	msg := &providerTypes.Message{
		EventType: events.RevertMessage,
		Sn:        sn.Uint64(),
	}
	tx, err := p.SendTransaction(ctx, opts, msg, providerTypes.MaxTxRetry)
	if err != nil {
		return err
	}
	receipt, err := p.WaitForResults(ctx, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return fmt.Errorf("failed to revert message: %s", err)
	}
	return nil
}

// ClaimFees
func (p *Provider) ClaimFee(ctx context.Context) error {
	msg := &providerTypes.Message{
		EventType: events.ClaimFee,
	}
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	tx, err := p.SendTransaction(ctx, opts, msg, providerTypes.MaxTxRetry)
	if err != nil {
		return err
	}
	receipt, err := p.WaitForResults(ctx, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return fmt.Errorf("failed to revert message: %s", err)
	}
	return nil
}

// SetFee
func (p *Provider) SetFee(ctx context.Context, networkID string, msgFee, resFee *big.Int) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	msg := &providerTypes.Message{
		EventType: events.SetFee,
		Src:       networkID,
		Sn:        msgFee.Uint64(),
		ReqID:     resFee.Uint64(),
	}
	tx, err := p.SendTransaction(ctx, opts, msg, providerTypes.MaxTxRetry)
	if err != nil {
		return err
	}
	receipt, err := p.WaitForResults(ctx, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return fmt.Errorf("failed to set fee: %s", err)
	}
	return nil
}

// GetFee
func (p *Provider) GetFee(ctx context.Context, networkID string) (uint64, error) {
	fee, err := p.client.GetFee(&bind.CallOpts{Context: ctx}, networkID)
	if err != nil {
		return 0, err
	}
	return fee.Uint64(), nil
}

// ExecuteRollback
func (p *Provider) ExecuteRollback(ctx context.Context, sn uint64) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	msg := &providerTypes.Message{
		EventType: events.ExecuteRollback,
		Sn:        sn,
	}
	tx, err := p.SendTransaction(ctx, opts, msg, providerTypes.MaxTxRetry)
	if err != nil {
		return err
	}
	receipt, err := p.WaitForResults(ctx, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return fmt.Errorf("failed to execute rollback: %s", err)
	}
	return nil
}

// EstimateGas
func (p *Provider) EstimateGas(ctx context.Context, message *providerTypes.Message) (uint64, error) {
	contract := common.HexToAddress(p.cfg.Contracts[providerTypes.ConnectionContract])
	msg := ethereum.CallMsg{
		From: p.wallet.Address,
		To:   &contract,
	}
	switch message.EventType {
	case events.EmitMessage:
		abi, err := bridgeContract.ConnectionMetaData.GetAbi()
		if err != nil {
			return 0, err
		}
		data, err := abi.Pack(MethodRecvMessage, message.Src, message.Sn, message.Data)
		if err != nil {
			return 0, nil
		}
		msg.Data = data
	case events.CallMessage:
		abi, err := bridgeContract.XcallMetaData.GetAbi()
		if err != nil {
			return 0, err
		}
		data, err := abi.Pack(MethodExecuteCall, message.ReqID, message.Data)
		if err != nil {
			return 0, nil
		}
		msg.Data = data
		contract = common.HexToAddress(p.cfg.Contracts[providerTypes.XcallContract])
	}
	return p.client.EstimateGas(ctx, msg)
}
