package wasm

import (
	"context"
	"fmt"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	wasmTypes "github.com/CosmWasm/wasmd/x/wasm/types"
	coreTypes "github.com/cometbft/cometbft/rpc/core/types"
	sdkTypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/icon-project/centralized-relay/relayer/chains/wasm/types"
	"github.com/icon-project/centralized-relay/relayer/events"
	"github.com/icon-project/centralized-relay/relayer/kms"
	"github.com/icon-project/centralized-relay/relayer/provider"
	relayTypes "github.com/icon-project/centralized-relay/relayer/types"
	jsoniter "github.com/json-iterator/go"
	"go.uber.org/zap"
)

type Provider struct {
	logger    *zap.Logger
	cfg       *ProviderConfig
	client    IClient
	kms       kms.KMS
	wallet    sdkTypes.AccountI
	contracts map[string]relayTypes.EventMap
	eventList []sdkTypes.Event
}

func (p *Provider) QueryLatestHeight(ctx context.Context) (uint64, error) {
	return p.client.GetLatestBlockHeight(ctx)
}

func (p *Provider) QueryTransactionReceipt(ctx context.Context, txHash string) (*relayTypes.Receipt, error) {
	res, err := p.client.GetTransactionReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	return &relayTypes.Receipt{
		TxHash: txHash,
		Height: uint64(res.TxResponse.Height),
		Status: types.CodeTypeOK == res.TxResponse.Code,
	}, nil
}

func (p *Provider) NID() string {
	return p.cfg.NID
}

func (p *Provider) Name() string {
	return p.cfg.ChainName
}

func (p *Provider) Init(ctx context.Context, homePath string, kms kms.KMS) error {
	if err := p.cfg.Contracts.Validate(); err != nil {
		return err
	}
	p.kms = kms
	return nil
}

// Wallet returns the wallet of the provider
func (p *Provider) Wallet() sdkTypes.AccAddress {
	if p.wallet == nil {
		if err := p.RestoreKeystore(context.Background()); err != nil {
			p.logger.Error("failed to restore keystore", zap.Error(err))
			return nil
		}
		account, err := p.client.GetAccountInfo(context.Background(), p.cfg.GetWallet())
		if err != nil {
			p.logger.Error("failed to get account info", zap.Error(err))
			return nil
		}
		p.wallet = account
		return p.client.SetAddress(account.GetAddress())
	}
	return p.wallet.GetAddress()
}

func (p *Provider) Type() string {
	return types.ChainType
}

func (p *Provider) Config() provider.Config {
	return p.cfg
}

func (p *Provider) Listener(ctx context.Context, lastSavedHeight interface{}, blockInfoChan chan *relayTypes.BlockInfo) error {
	latestHeight, err := p.QueryLatestHeight(ctx)
	if err != nil {
		p.logger.Error("failed to get latest block height", zap.Error(err))
		return err
	}

	startHeight, err := p.getStartHeight(latestHeight, lastSavedHeight.(uint64))
	if err != nil {
		p.logger.Error("failed to determine start height", zap.Error(err))
		return err
	}

	subscribeStarter := time.NewTicker(time.Second * 1)

	p.logger.Info("Start from height", zap.Uint64("height", startHeight), zap.Uint64("finality block", p.FinalityBlock(ctx)))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-subscribeStarter.C:
			subscribeStarter.Stop()
			go p.SubscribeMessageEvents(ctx, blockInfoChan, &types.SubscribeOpts{
				Address: p.cfg.Contracts[relayTypes.ConnectionContract],
				Method:  EventTypeWasmMessage,
				Height:  latestHeight,
			})
			go p.SubscribeMessageEvents(ctx, blockInfoChan, &types.SubscribeOpts{
				Address: p.cfg.Contracts[relayTypes.XcallContract],
				Method:  EventTypeWasmCallMessage,
				Height:  latestHeight,
			})
		default:
			if startHeight < latestHeight {
				p.logger.Debug("Query started", zap.Uint64("from-height", startHeight), zap.Uint64("to-height", latestHeight))
				startHeight = p.runBlockQuery(ctx, blockInfoChan, startHeight, latestHeight)
			}
		}
	}
}

func (p *Provider) Route(ctx context.Context, message *relayTypes.Message, callback relayTypes.TxResponseFunc) error {
	p.logger.Info("starting to route message", zap.Any("message", message))
	res, err := p.call(ctx, message)
	if err != nil {
		return err
	}
	seq := p.wallet.GetSequence() + 1
	if err := p.wallet.SetSequence(seq); err != nil {
		p.logger.Error("failed to set sequence", zap.Error(err))
	}
	p.waitForTxResult(ctx, message.MessageKey(), res.TxHash, callback)
	return nil
}

// call the smart contract to send the message
func (p *Provider) call(ctx context.Context, message *relayTypes.Message) (*sdkTypes.TxResponse, error) {
	rawMsg, err := p.getRawContractMessage(message)
	if err != nil {
		return nil, err
	}

	var contract string

	switch message.EventType {
	case events.EmitMessage, events.RevertMessage, events.SetAdmin, events.ClaimFee, events.SetFee:
		contract = p.cfg.Contracts[relayTypes.ConnectionContract]
	case events.CallMessage, events.ExecuteRollback:
		contract = p.cfg.Contracts[relayTypes.XcallContract]
	default:
		return nil, fmt.Errorf("unknown event type: %s ", message.EventType)
	}

	msg := &wasmTypes.MsgExecuteContract{
		Sender:   p.Wallet().String(),
		Contract: contract,
		Msg:      rawMsg,
	}

	msgs := []sdkTypes.Msg{msg}

	res, err := p.sendMessage(ctx, msgs...)
	if err != nil {
		if strings.Contains(err.Error(), errors.ErrWrongSequence.Error()) {
			if mmErr := p.handleSequence(ctx); mmErr != nil {
				return nil, fmt.Errorf("failed to handle sequence mismatch error: %v || %v", mmErr, err)
			}
			return p.sendMessage(ctx, msgs...)
		}
	}
	return res, err
}

func (p *Provider) sendMessage(ctx context.Context, msgs ...sdkTypes.Msg) (*sdkTypes.TxResponse, error) {
	return p.prepareAndPushTxToMemPool(ctx, p.wallet.GetAccountNumber(), p.wallet.GetSequence(), msgs...)
}

func (p *Provider) handleSequence(ctx context.Context) error {
	acc, err := p.client.GetAccountInfo(ctx, p.Wallet().String())
	if err != nil {
		return err
	}
	return p.wallet.SetSequence(acc.GetSequence())
}

func (p *Provider) logTxFailed(err error, txHash string) {
	p.logger.Error("transaction failed",
		zap.Error(err),
		zap.String("tx_hash", txHash),
	)
}

func (p *Provider) logTxSuccess(height uint64, txHash string) {
	p.logger.Info("successful transaction",
		zap.Uint64("block_height", height),
		zap.String("chain_id", p.cfg.ChainID),
		zap.String("tx_hash", txHash),
	)
}

func (p *Provider) prepareAndPushTxToMemPool(ctx context.Context, acc, seq uint64, msgs ...sdkTypes.Msg) (*sdkTypes.TxResponse, error) {
	txf, err := p.client.BuildTxFactory()
	if err != nil {
		return nil, err
	}

	txf = txf.
		WithGasPrices(p.cfg.GasPrices).
		WithGasAdjustment(p.cfg.GasAdjustment).
		WithAccountNumber(acc).
		WithSequence(seq)

	if txf.SimulateAndExecute() {
		_, adjusted, err := p.client.EstimateGas(txf, msgs...)
		if err != nil {
			return nil, err
		}
		txf = txf.WithGas(adjusted)
	}

	if txf.Gas() < p.cfg.MinGasAmount {
		return nil, fmt.Errorf("gas amount %d is too low; the minimum allowed gas amount is %d", txf.Gas(), p.cfg.MinGasAmount)
	}

	if txf.Gas() > p.cfg.MaxGasAmount {
		return nil, fmt.Errorf("gas amount %d exceeds the maximum allowed limit of %d", txf.Gas(), p.cfg.MaxGasAmount)
	}

	txBytes, err := p.client.PrepareTx(ctx, txf, msgs...)
	if err != nil {
		return nil, err
	}

	res, err := p.client.BroadcastTx(txBytes)
	if err != nil || res.Code != types.CodeTypeOK {
		if err == nil {
			err = fmt.Errorf("failed to send tx: %v", res.RawLog)
		}
		return nil, err
	}

	return res, nil
}

func (p *Provider) waitForTxResult(ctx context.Context, mk *relayTypes.MessageKey, txHash string, callback relayTypes.TxResponseFunc) {
	for txWaitRes := range p.subscribeTxResultStream(ctx, txHash, p.cfg.TxConfirmationInterval) {
		if txWaitRes.Error != nil && txWaitRes.Error != context.DeadlineExceeded {
			p.logTxFailed(txWaitRes.Error, txHash)
			callback(mk, txWaitRes.TxResult, txWaitRes.Error)
			return
		}
		p.logTxSuccess(uint64(txWaitRes.TxResult.Height), txHash)
		callback(mk, txWaitRes.TxResult, nil)
	}
}

func (p *Provider) pollTxResultStream(ctx context.Context, txHash string, maxWaitInterval time.Duration) <-chan *types.TxResultChan {
	txResChan := make(chan *types.TxResultChan)
	startTime := time.Now()
	go func(txChan chan *types.TxResultChan) {
		defer close(txChan)
		for range time.NewTicker(p.cfg.TxConfirmationInterval).C {
			res, err := p.client.GetTransactionReceipt(ctx, txHash)
			if err == nil {
				txChan <- &types.TxResultChan{
					TxResult: &relayTypes.TxResponse{
						Height:    res.TxResponse.Height,
						TxHash:    res.TxResponse.TxHash,
						Codespace: res.TxResponse.Codespace,
						Code:      relayTypes.ResponseCode(res.TxResponse.Code),
						Data:      res.TxResponse.Data,
					},
				}
				return
			} else if time.Since(startTime) > maxWaitInterval {
				txChan <- &types.TxResultChan{
					Error: err,
				}
				return
			}
		}
	}(txResChan)
	return txResChan
}

func (p *Provider) subscribeTxResultStream(ctx context.Context, txHash string, maxWaitInterval time.Duration) <-chan *types.TxResultChan {
	txResChan := make(chan *types.TxResultChan)
	go func(txRes chan *types.TxResultChan) {
		defer close(txRes)

		newCtx, cancel := context.WithTimeout(ctx, maxWaitInterval)
		defer cancel()

		query := fmt.Sprintf("tm.event = 'Tx' AND tx.hash = '%s'", txHash)
		resultEventChan, err := p.client.Subscribe(newCtx, "tx-result-waiter", query)
		if err != nil {
			txRes <- &types.TxResultChan{
				TxResult: &relayTypes.TxResponse{
					TxHash: txHash,
				},
				Error: err,
			}
			return
		}
		defer p.client.Unsubscribe(newCtx, "tx-result-waiter", query)

		for {
			select {
			case <-ctx.Done():
				return
			case e := <-resultEventChan:
				eventDataJSON, err := jsoniter.Marshal(e.Data)
				if err != nil {
					txRes <- &types.TxResultChan{
						TxResult: &relayTypes.TxResponse{
							TxHash: txHash,
						}, Error: err,
					}
					return
				}

				txWaitRes := new(types.TxResultWaitResponse)
				if err := jsoniter.Unmarshal(eventDataJSON, txWaitRes); err != nil {
					txRes <- &types.TxResultChan{
						TxResult: &relayTypes.TxResponse{
							TxHash: txHash,
						}, Error: err,
					}
					return
				}
				if uint32(txWaitRes.Result.Code) != types.CodeTypeOK {
					txRes <- &types.TxResultChan{
						Error: fmt.Errorf(txWaitRes.Result.Log),
						TxResult: &relayTypes.TxResponse{
							Height:    txWaitRes.Height,
							TxHash:    txHash,
							Codespace: txWaitRes.Result.Codespace,
							Code:      relayTypes.ResponseCode(txWaitRes.Result.Code),
							Data:      string(txWaitRes.Result.Data),
						},
					}
					return
				}

				txRes <- &types.TxResultChan{
					TxResult: &relayTypes.TxResponse{
						Height:    txWaitRes.Height,
						TxHash:    txHash,
						Codespace: txWaitRes.Result.Codespace,
						Code:      relayTypes.ResponseCode(txWaitRes.Result.Code),
						Data:      string(txWaitRes.Result.Data),
					},
				}
				return
			}
		}
	}(txResChan)
	return txResChan
}

func (p *Provider) MessageReceived(ctx context.Context, key *relayTypes.MessageKey) (bool, error) {
	queryMsg := &types.QueryReceiptMsg{
		GetReceipt: &types.GetReceiptMsg{
			SrcNetwork: key.Src,
			ConnSn:     strconv.FormatUint(key.Sn, 10),
		},
	}
	rawQueryMsg, err := jsoniter.Marshal(queryMsg)
	if err != nil {
		return false, err
	}

	res, err := p.client.QuerySmartContract(ctx, p.cfg.Contracts[relayTypes.ConnectionContract], rawQueryMsg)
	if err != nil {
		p.logger.Error("failed to check if message is received: ", zap.Error(err))
		return false, err
	}

	receiptMsgRes := types.QueryReceiptMsgResponse{}
	return receiptMsgRes.Status, jsoniter.Unmarshal(res.Data, &receiptMsgRes.Status)
}

func (p *Provider) QueryBalance(ctx context.Context, addr string) (*relayTypes.Coin, error) {
	coin, err := p.client.GetBalance(ctx, addr, p.cfg.Denomination)
	if err != nil {
		p.logger.Error("failed to query balance: ", zap.Error(err))
		return nil, err
	}
	return &relayTypes.Coin{
		Denom:  coin.Denom,
		Amount: coin.Amount.BigInt().Uint64(),
	}, nil
}

func (p *Provider) ShouldReceiveMessage(ctx context.Context, message *relayTypes.Message) (bool, error) {
	return true, nil
}

func (p *Provider) ShouldSendMessage(ctx context.Context, message *relayTypes.Message) (bool, error) {
	return true, nil
}

func (p *Provider) GenerateMessages(ctx context.Context, messageKey *relayTypes.MessageKeyWithMessageHeight) ([]*relayTypes.Message, error) {
	blocks, err := p.fetchBlockMessages(ctx, &types.HeightRange{messageKey.Height, messageKey.Height})
	if err != nil {
		return nil, err
	}
	var messages []*relayTypes.Message
	for _, block := range blocks {
		messages = append(messages, block.Messages...)
	}
	return messages, nil
}

func (p *Provider) FinalityBlock(ctx context.Context) uint64 {
	return p.cfg.FinalityBlock
}

func (p *Provider) RevertMessage(ctx context.Context, sn *big.Int) error {
	msg := &relayTypes.Message{
		Sn:        sn.Uint64(),
		EventType: events.RevertMessage,
	}
	_, err := p.call(ctx, msg)
	return err
}

// SetFee
func (p *Provider) SetFee(ctx context.Context, networkdID string, msgFee, resFee uint64) error {
	msg := &relayTypes.Message{
		Src:       networkdID,
		Sn:        msgFee,
		ReqID:     resFee,
		EventType: events.SetFee,
	}
	_, err := p.call(ctx, msg)
	return err
}

// ClaimFee
func (p *Provider) ClaimFee(ctx context.Context) error {
	msg := &relayTypes.Message{
		EventType: events.ClaimFee,
	}
	_, err := p.call(ctx, msg)
	return err
}

// GetFee returns the fee for the given networkID
// responseFee is used to determine if the fee should be returned
func (p *Provider) GetFee(ctx context.Context, networkID string, responseFee bool) (uint64, error) {
	getFee := types.NewExecGetFee(networkID, responseFee)
	data, err := jsoniter.Marshal(getFee)
	if err != nil {
		return 0, err
	}
	return p.client.GetFee(ctx, p.cfg.Contracts[relayTypes.ConnectionContract], data)
}

func (p *Provider) SetAdmin(ctx context.Context, address string) error {
	msg := &relayTypes.Message{
		Src:       address,
		EventType: events.SetAdmin,
	}
	_, err := p.call(ctx, msg)
	return err
}

// ExecuteRollback
func (p *Provider) ExecuteRollback(ctx context.Context, sn *big.Int) error {
	msg := &relayTypes.Message{
		Sn:        sn.Uint64(),
		EventType: events.ExecuteRollback,
	}
	_, err := p.call(ctx, msg)
	return err
}

func (p *Provider) getStartHeight(latestHeight, lastSavedHeight uint64) (uint64, error) {
	startHeight := lastSavedHeight
	if p.cfg.StartHeight > 0 && p.cfg.StartHeight < latestHeight {
		return p.cfg.StartHeight, nil
	}

	if startHeight > latestHeight {
		return 0, fmt.Errorf("last saved height cannot be greater than latest height")
	}

	if startHeight != 0 && startHeight < latestHeight {
		return startHeight, nil
	}

	return latestHeight, nil
}

func (p *Provider) getHeightStream(done <-chan bool, fromHeight, toHeight uint64) <-chan *types.HeightRange {
	heightChan := make(chan *types.HeightRange)
	go func(fromHeight, toHeight uint64, heightChan chan *types.HeightRange) {
		defer close(heightChan)
		for fromHeight < toHeight {
			select {
			case <-done:
				return
			case heightChan <- &types.HeightRange{Start: fromHeight, End: fromHeight + 2}:
				fromHeight += 2
			}
		}
	}(fromHeight, toHeight, heightChan)
	return heightChan
}

func (p *Provider) getBlockInfoStream(ctx context.Context, done <-chan bool, heightStreamChan <-chan *types.HeightRange) <-chan interface{} {
	blockInfoStream := make(chan interface{})
	go func(blockInfoChan chan interface{}, heightChan <-chan *types.HeightRange) {
		defer close(blockInfoChan)
		for {
			select {
			case <-done:
				return
			case height, ok := <-heightChan:
				if ok {
					for {
						messages, err := p.fetchBlockMessages(ctx, height)
						if err != nil {
							p.logger.Error("failed to fetch block messages", zap.Error(err), zap.Any("height", height))
							time.Sleep(time.Second * 3)
						} else {
							for _, message := range messages {
								blockInfoChan <- message
							}
							break
						}
					}
				}
			}
		}
	}(blockInfoStream, heightStreamChan)
	return blockInfoStream
}

func (p *Provider) fetchBlockMessages(ctx context.Context, heightInfo *types.HeightRange) ([]*relayTypes.BlockInfo, error) {
	perPage := 25
	searchParam := types.TxSearchParam{
		StartHeight: heightInfo.Start,
		EndHeight:   heightInfo.End,
		PerPage:     &perPage,
	}

	var (
		wg           sync.WaitGroup
		messages     coreTypes.ResultTxSearch
		messagesChan = make(chan *coreTypes.ResultTxSearch)
		errorChan    = make(chan error)
	)

	for _, event := range p.eventList {
		wg.Add(1)
		go func(wg *sync.WaitGroup, searchParam types.TxSearchParam, messagesChan chan *coreTypes.ResultTxSearch, errorChan chan error) {
			defer wg.Done()
			searchParam.Events = append(searchParam.Events, event)
			res, err := p.client.TxSearch(ctx, searchParam)
			if err != nil {
				errorChan <- err
				return
			}
			if res.TotalCount > perPage {
				for i := 2; i <= int(res.TotalCount/perPage)+1; i++ {
					searchParam.Page = &i
					resNext, err := p.client.TxSearch(ctx, searchParam)
					if err != nil {
						errorChan <- err
						return
					}
					res.Txs = append(res.Txs, resNext.Txs...)
				}
			}
			messagesChan <- res
		}(&wg, searchParam, messagesChan, errorChan)
		select {
		case msgs := <-messagesChan:
			messages.Txs = append(messages.Txs, msgs.Txs...)
			messages.TotalCount += msgs.TotalCount
		case err := <-errorChan:
			p.logger.Error("failed to fetch block messages", zap.Error(err))
		}
	}
	wg.Wait()
	return p.getMessagesFromTxList(messages.Txs)
}

func (p *Provider) getMessagesFromTxList(resultTxList []*coreTypes.ResultTx) ([]*relayTypes.BlockInfo, error) {
	var messages []*relayTypes.BlockInfo
	for _, resultTx := range resultTxList {
		var eventsList []*EventsList
		if err := jsoniter.Unmarshal([]byte(resultTx.TxResult.Log), &eventsList); err != nil {
			return nil, err
		}

		for _, event := range eventsList {
			msgs, err := p.ParseMessageFromEvents(event.Events)
			if err != nil {
				return nil, err
			}
			for _, msg := range msgs {
				msg.MessageHeight = uint64(resultTx.Height)
				p.logger.Info("Detected eventlog",
					zap.Uint64("height", msg.MessageHeight),
					zap.String("target_network", msg.Dst),
					zap.Uint64("sn", msg.Sn),
					zap.String("event_type", msg.EventType),
				)
			}
			messages = append(messages, &relayTypes.BlockInfo{
				Height:   uint64(resultTx.Height),
				Messages: msgs,
			})
		}
	}
	return messages, nil
}

func (p *Provider) getRawContractMessage(message *relayTypes.Message) (wasmTypes.RawContractMessage, error) {
	switch message.EventType {
	case events.EmitMessage:
		rcvMsg := types.NewExecRecvMsg(message)
		return jsoniter.Marshal(rcvMsg)
	case events.CallMessage:
		execMsg := types.NewExecExecMsg(message)
		return jsoniter.Marshal(execMsg)
	case events.RevertMessage:
		revertMsg := types.NewExecRevertMsg(message)
		return jsoniter.Marshal(revertMsg)
	case events.SetAdmin:
		setAdmin := types.NewExecSetAdmin(message.Dst)
		return jsoniter.Marshal(setAdmin)
	case events.ClaimFee:
		claimFee := types.NewExecClaimFee()
		return jsoniter.Marshal(claimFee)
	case events.SetFee:
		setFee := types.NewExecSetFee(message.Src, message.Sn, message.ReqID)
		return jsoniter.Marshal(setFee)
	case events.ExecuteRollback:
		executeRollback := types.NewExecExecuteRollback(message.Sn)
		return jsoniter.Marshal(executeRollback)
	default:
		return nil, fmt.Errorf("unknown event type: %s ", message.EventType)
	}
}

func (p *Provider) getNumOfPipelines(diff int) int {
	if diff <= runtime.NumCPU() {
		return diff
	}
	return runtime.NumCPU() / 2
}

func (p *Provider) runBlockQuery(ctx context.Context, blockInfoChan chan *relayTypes.BlockInfo, fromHeight, toHeight uint64) uint64 {
	done := make(chan bool)
	defer close(done)

	heightStream := p.getHeightStream(done, fromHeight, toHeight)

	diff := int(toHeight-fromHeight) / 2

	numOfPipelines := p.getNumOfPipelines(diff)
	wg := &sync.WaitGroup{}
	for i := 0; i < numOfPipelines; i++ {
		wg.Add(1)
		go func(wg *sync.WaitGroup, heightStream <-chan *types.HeightRange) {
			defer wg.Done()
			for heightRange := range heightStream {
				blockInfo, err := p.fetchBlockMessages(ctx, heightRange)
				if err != nil {
					p.logger.Error("failed to fetch block messages", zap.Error(err))
					continue
				}
				var messages []*relayTypes.Message
				for _, block := range blockInfo {
					messages = append(messages, block.Messages...)
				}
				blockInfoChan <- &relayTypes.BlockInfo{
					Height:   heightRange.End,
					Messages: messages,
				}
			}
		}(wg, heightStream)
	}
	wg.Wait()
	return toHeight + 1
}

// SubscribeMessageEvents subscribes to the message events
// Expermental: Allows to subscribe to the message events realtime without fully syncing the chain
func (p *Provider) SubscribeMessageEvents(ctx context.Context, blockInfoChan chan *relayTypes.BlockInfo, opts *types.SubscribeOpts) error {
	query := strings.Join([]string{
		"tm.event = 'Tx'",
		fmt.Sprintf("tx.height >= %d ", opts.Height),
		fmt.Sprintf("%s._contract_address = '%s'", opts.Method, opts.Address),
	}, " AND ")

	resultEventChan, err := p.client.Subscribe(ctx, "tx-result-waiter", query)
	if err != nil {
		p.logger.Error("event subscription failed", zap.Error(err))
		return p.SubscribeMessageEvents(ctx, blockInfoChan, opts)
	}
	defer p.client.Unsubscribe(ctx, opts.Address, query)
	p.logger.Info("event subscription started", zap.String("contract_address", opts.Address), zap.String("method", opts.Method))

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("event subscription stopped")
			return ctx.Err()
		case e := <-resultEventChan:
			eventDataJSON, err := jsoniter.Marshal(e.Data)
			if err != nil {
				p.logger.Error("failed to marshal event data", zap.Error(err))
				continue
			}
			var res types.TxResultWaitResponse
			if err := jsoniter.Unmarshal(eventDataJSON, &res); err != nil {
				p.logger.Error("failed to unmarshal event data", zap.Error(err))
				continue
			}
			eventsList := []struct {
				Events []Event `json:"events"`
			}{}
			if err := jsoniter.Unmarshal([]byte(res.Result.Log), &eventsList); err != nil {
				p.logger.Error("failed to unmarshal event list", zap.Error(err))
				continue
			}
			var messages []*relayTypes.Message
			for _, event := range eventsList {
				msgs, err := p.ParseMessageFromEvents(event.Events)
				if err != nil {
					p.logger.Error("failed to parse message from events", zap.Error(err))
					continue
				}
				messages = append(messages, msgs...)
			}

			blockInfo := &relayTypes.BlockInfo{
				Height:   uint64(res.Height),
				Messages: messages,
			}
			blockInfoChan <- blockInfo
			opts.Height = blockInfo.Height
			for _, msg := range blockInfo.Messages {
				p.logger.Info("Detected eventlog",
					zap.Int64("height", res.Height),
					zap.String("target_network", msg.Dst),
					zap.Uint64("sn", msg.Sn),
					zap.String("event_type", msg.EventType),
				)
			}
		default:
			if !p.client.IsConnected() {
				p.logger.Warn("http client stopped")
				if err := p.client.Reconnect(); err != nil {
					p.logger.Warn("failed to reconnect", zap.Error(err))
					time.Sleep(time.Second * 1)
					continue
				}
				p.logger.Debug("http client reconnected")
				return p.SubscribeMessageEvents(ctx, blockInfoChan, opts)
			}
		}
	}
}
