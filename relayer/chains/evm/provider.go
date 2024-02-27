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
	"github.com/ethereum/go-ethereum/core/types"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/icon-project/centralized-relay/relayer/kms"
	"github.com/icon-project/centralized-relay/relayer/provider"
	providerTypes "github.com/icon-project/centralized-relay/relayer/types"

	"go.uber.org/zap"
)

var _ provider.Config = (*EVMProviderConfig)(nil)

type EVMProviderConfig struct {
	ChainName      string                          `json:"-" yaml:"-"`
	RPCUrl         string                          `json:"rpc-url" yaml:"rpc-url"`
	VerifierRPCUrl string                          `json:"verifier-rpc-url" yaml:"verifier-rpc-url"`
	StartHeight    uint64                          `json:"start-height" yaml:"start-height"`
	Address        string                          `json:"address" yaml:"address"`
	GasMin         uint64                          `json:"gas-min" yaml:"gas-min"`
	GasLimit       uint64                          `json:"gas-limit" yaml:"gas-limit"`
	Contracts      providerTypes.ContractConfigMap `json:"contracts" yaml:"contracts"`
	Concurrency    uint64                          `json:"concurrency" yaml:"concurrency"`
	FinalityBlock  uint64                          `json:"finality-block" yaml:"finality-block"`
	BlockInterval  time.Duration                   `json:"block-interval" yaml:"block-interval"`
	NID            string                          `json:"nid" yaml:"nid"`
	HomeDir        string                          `json:"-" yaml:"-"`
}

type EVMProvider struct {
	client      IClient
	verifier    IClient
	log         *zap.Logger
	cfg         *EVMProviderConfig
	StartHeight uint64
	blockReq    ethereum.FilterQuery
	wallet      *keystore.Key
	kms         kms.KMS
	contracts   map[string]providerTypes.EventMap
}

func (p *EVMProviderConfig) NewProvider(ctx context.Context, log *zap.Logger, homepath string, debug bool, chainName string) (provider.ChainProvider, error) {
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

	return &EVMProvider{
		cfg:       p,
		log:       log.With(zap.Stringp("nid", &p.NID), zap.Stringp("name", &p.ChainName)),
		client:    client,
		blockReq:  p.GetMonitorEventFilters(),
		verifier:  verifierClient,
		contracts: p.eventMap(),
	}, nil
}

func (p *EVMProvider) NID() string {
	return p.cfg.NID
}

func (p *EVMProviderConfig) Validate() error {
	if err := p.Contracts.Validate(); err != nil {
		return fmt.Errorf("contracts are not valid: %s", err)
	}
	return nil
}

func (p *EVMProviderConfig) SetWallet(addr string) {
	p.Address = addr
}

func (p *EVMProviderConfig) GetWallet() string {
	return p.Address
}

func (p *EVMProvider) Init(ctx context.Context, homePath string, kms kms.KMS) error {
	p.kms = kms
	return nil
}

func (p *EVMProvider) Type() string {
	return "evm"
}

func (p *EVMProvider) Config() provider.Config {
	return p.cfg
}

func (p *EVMProvider) Name() string {
	return p.cfg.ChainName
}

func (p *EVMProvider) Wallet() (*keystore.Key, error) {
	if p.wallet == nil {
		if err := p.RestoreKeystore(context.Background()); err != nil {
			return nil, err
		}
	}
	return p.wallet, nil
}

func (p *EVMProvider) FinalityBlock(ctx context.Context) uint64 {
	return p.cfg.FinalityBlock
}

func (p *EVMProvider) WaitForResults(ctx context.Context, txHash common.Hash) (txr *ethTypes.Receipt, err error) {
	ticker := time.NewTicker(DefaultGetTransactionResultPollingInterval * time.Millisecond)
	var retryCounter uint8
	for {
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			err = ctx.Err()
			return
		case <-ticker.C:
			if retryCounter >= providerTypes.MaxTxRetry {
				err = fmt.Errorf("Max retry reached for tx %s", txHash.String())
				return
			}
			retryCounter++
			txr, err = p.client.TransactionReceipt(ctx, txHash)
			if err != nil && err == ethereum.NotFound {
				continue
			}
			return
		}
	}
}

func (r *EVMProvider) transferBalance(senderKey, recepientAddress string, amount *big.Int) (txnHash common.Hash, err error) {
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
	tx := types.NewTransaction(nonce.Uint64(), common.HexToAddress(recepientAddress), amount, 30000000, gasPrice, []byte{})
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), from)
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

func (p *EVMProvider) GetTransationOpts(ctx context.Context) (*bind.TransactOpts, error) {
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

	non, err := p.client.NonceAt(ctx, wallet.Address, nil)
	if err != nil {
		return nil, err
	}

	txOpts, err := newTransactOpts(p.wallet)
	if err != nil {
		return nil, err
	}
	txOpts.Nonce = non
	txOpts.Context = ctx
	if p.cfg.GasLimit > 0 {
		txOpts.GasPrice = big.NewInt(int64(p.cfg.GasLimit))
	}

	return txOpts, nil
}

// SetAdmin sets the admin address of the bridge contract
func (p *EVMProvider) SetAdmin(ctx context.Context, admin string) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	tx, err := p.client.SetAdmin(opts, common.HexToAddress(admin))
	if err != nil {
		return err
	}
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
func (p *EVMProvider) RevertMessage(ctx context.Context, sn *big.Int) error {
	opts, err := p.GetTransationOpts(ctx)
	if err != nil {
		return err
	}
	tx, err := p.client.RevertMessage(opts, sn)
	if err != nil {
		return err
	}
	res, err := p.WaitForResults(ctx, tx.Hash())
	if res.Status != 1 {
		return fmt.Errorf("failed to revert message: %s", err)
	}
	return err
}
