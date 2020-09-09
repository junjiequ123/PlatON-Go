package eth

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"os"
	"sync/atomic"
	"time"

	"github.com/PlatONnetwork/PlatON-Go/core"

	"github.com/PlatONnetwork/PlatON-Go/rlp"

	"github.com/PlatONnetwork/PlatON-Go/crypto/sha3"

	"github.com/PlatONnetwork/PlatON-Go/core/rawdb"

	"github.com/mroth/weightedrand"

	"github.com/PlatONnetwork/PlatON-Go/event"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
	"github.com/PlatONnetwork/PlatON-Go/crypto"
	"github.com/PlatONnetwork/PlatON-Go/log"
)

const (
	waitBLockTime = time.Second * 10
)

func NewTxGenAPI(eth *Ethereum) *TxGenAPI {
	return &TxGenAPI{eth: eth}
}

type BlockInfo struct {
	ProduceTime int64 `json:"id_time"`
	Number      int64 `json:"block"`
	TxLength    int   `json:"tx_length"`
	TimeUse     int64 `json:"time_use"`
}

type BlockInfos []BlockInfo

func (t BlockInfos) Len() int {
	return len(t)
}

func (t BlockInfos) Less(i, j int) bool {
	if t[i].ProduceTime < (t[j].ProduceTime) {
		return true
	}
	return false
}

func (t BlockInfos) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

type TxGenResData struct {
	Ttf         []BlockInfo `json:"ttf"`
	TotalTxSend uint64      `json:"total_tx_send"`
}

type TxGenAPI struct {
	eth              *Ethereum
	txGenExitCh      chan struct{}
	txGenStopTxCh    chan struct{}
	start            bool
	blockfeed        event.Subscription
	blockExecuteFeed event.Subscription

	res *TxGenResData
}

// Start, begin make tx ,Broadcast transactions directly through p2p, without entering the transactin pool
// normalTx, evmTx, wasmTx，The proportion of normal transactions and contract transactions sent out
// such as 1:1:1,this should send 1 normal transactions,1 evm transaction, 1 wasm transaction
// totalTxPer,How many transactions are sent at once
// activeTxPer,How many active transactions are sent at once,  active tx per + no active tx per = totalTxPer
// txFrequency,every time(ms) to send totalTxPer of transactions
// activeSender,the amount of active accounts,this should not greater than total accounts
// sendingAmount,Send amount of normal transaction
// accountPath,Account configuration address
// start, end ,Start and end account
// max wait account receipt time,seconds.if not receive the account receipt ,will resend the tx
func (txg *TxGenAPI) Start(normalTx, evmTx, wasmTx uint, totalTxPer, activeTxPer, txFrequency, activeSender uint, sendingAmount uint64, accountPath string, start, end uint, waitAccountReceiptTime uint) error {
	if txg.start {
		return errors.New("the tx maker is working")
	}

	//make sure when the txGen is start ,the node will not receive txs from other node,
	//so this node can keep in sync with other nodes
	atomic.StoreUint32(&txg.eth.protocolManager.acceptRemoteTxs, 1)

	blockch := make(chan *types.Block, 200)
	txg.blockfeed = txg.eth.blockchain.SubscribeWriteStateBlocksEvent(blockch)

	blockExecutech := make(chan *types.Block, 200)
	txg.blockExecuteFeed = txg.eth.blockchain.SubscribeExecuteBlocksEvent(blockExecutech)

	txg.txGenExitCh = make(chan struct{})
	txg.txGenStopTxCh = make(chan struct{})
	txg.res = new(TxGenResData)
	txg.res.TotalTxSend = 0
	txg.res.Ttf = make([]BlockInfo, 0)
	if err := txg.makeTransaction(normalTx, evmTx, wasmTx, totalTxPer, activeTxPer, txFrequency, activeSender, sendingAmount, accountPath, start, end, blockExecutech, blockch, time.Second*time.Duration(waitAccountReceiptTime)); err != nil {
		return err
	}
	txg.start = true
	return nil
}

func (txg *TxGenAPI) makeTransaction(tx, evm, wasm uint, totalTxPer, activeTxPer, txFrequency, activeSender uint, sendingAmount uint64, accountPath string, start, end uint, blockExcuteCh, blockQCCh chan *types.Block, waitAccountReceiptTime time.Duration) error {
	state, err := txg.eth.blockchain.State()
	if err != nil {
		return err
	}
	txm, err := NewTxMakeManger(tx, evm, wasm, totalTxPer, activeTxPer, txFrequency, activeSender, sendingAmount, txg.eth.txPool.Nonce, state.GetCodeSize, accountPath, start, end)
	if err != nil {
		state.ClearReference()
		return err
	}
	state.ClearReference()

	singine := types.NewEIP155Signer(new(big.Int).SetInt64(txg.eth.chainConfig.ChainID.Int64()))

	txsCh := make(chan []*types.Transaction, 2)
	go func() {
		ev := core.NewTxsEvent{}
		for {
			select {
			case txs := <-txsCh:
				//txg.eth.txPool.AddRemotes(txs)
				ev.Txs = txs
				txg.eth.protocolManager.txsCh <- ev
			case <-txg.txGenExitCh:
				log.Debug("MakeTransaction send tx exit")
				return
			}
		}
	}()

	log.Info("begin to MakeTransaction")
	gasPrice := txg.eth.txPool.GasPrice()

	shouldmake := time.NewTicker(time.Millisecond * time.Duration(txm.txFrequency))
	go func() {
		loopEnd := len(txm.accounts) + 100
		for {
			select {
			case res := <-blockExcuteCh:
				txm.blockProduceTime = time.Now()
				for _, receipt := range res.Transactions() {
					if account, ok := txm.accounts[receipt.FromAddr(singine)]; ok {
						if account.ReceiptsNonce < receipt.Nonce() {
							account.ReceiptsNonce = receipt.Nonce()
						}
					}
				}
				log.Debug("makeTx update receiptsNonce", "use", time.Since(txm.blockProduceTime), "txs", len(res.Transactions()))
				txm.blockCache[res.Hash()] = res
			case res := <-blockQCCh:
				now := time.Now()
				timeUse := int64(0)
				txm.blockProduceTime = now
				length := 0

				if block, ok := txm.blockCache[res.Hash()]; ok {
					for _, receipt := range block.Transactions() {
						if account, ok := txm.accounts[receipt.FromAddr(singine)]; ok {
							timeUse = timeUse + now.UnixNano() - account.SendTime[receipt.Nonce()].UnixNano()
							length++
							delete(account.SendTime, receipt.Nonce())
						}
					}
					delete(txm.blockCache, res.Hash())
				} else {
					for _, receipt := range res.Transactions() {
						if account, ok := txm.accounts[receipt.FromAddr(singine)]; ok {
							timeUse = timeUse + now.UnixNano() - account.SendTime[receipt.Nonce()].UnixNano()
							length++
							delete(account.SendTime, receipt.Nonce())
						}
					}
				}
				if length > 0 {
					txg.res.Ttf = append(txg.res.Ttf, BlockInfo{common.Millis(now), res.Number().Int64(), length, timeUse})
				}
				log.Debug("makeTx update res", "use", time.Since(now), "txs", len(res.Transactions()))
			case <-shouldmake.C:
				if time.Since(txm.blockProduceTime) >= waitBLockTime {
					log.Debug("makeTx should sleep", "time", time.Since(txm.blockProduceTime))
					continue
				}
				txs := make([]*types.Transaction, 0, txm.totalSenderTxPer)
				toAdd := txm.pickTxReceive()
				deactive := 0
				loop := 0

				now := time.Now()
				for len(txs) <= txm.totalSenderTxPer {
					if loop > loopEnd {
						break
					}
					loop++
					var account *txGenSendAccount
					if len(txs) < txm.activeSenderTxPer {
						account = txm.pickActiveSender()
					} else {
						account = txm.pickNormalSender()
					}
					if !account.active(waitAccountReceiptTime) {
						deactive++
						continue
					}

					txContractInputData, txReceive, gasLimit, amount := txm.generateTxParams(toAdd)

					tx := types.NewTransaction(account.Nonce, txReceive, amount, gasLimit, gasPrice, txContractInputData)
					newTx, err := types.SignTx(tx, singine, account.Priv)
					if err != nil {
						log.Crit(fmt.Errorf("sign error,%s", err.Error()).Error())
					}
					txg.res.TotalTxSend++
					txs = append(txs, newTx)
					txm.sendDone(account, now)
				}

				if len(txs) != 0 {
					txsCh <- txs
				}
				log.Debug("makeTx time use", "use", time.Since(now), "txs", len(txs), "deactive", deactive, "loop", loop)
			case <-txg.txGenStopTxCh:
				shouldmake.Stop()
				log.Debug("makeTx exit")
				return
			}
		}
	}()
	return nil
}

func (txg *TxGenAPI) GetTTF(latest uint64) ([]BlockInfo, error) {
	if latest == 0 {
		return txg.res.Ttf, nil
	}
	if int(latest) > len(txg.res.Ttf) {
		return nil, fmt.Errorf("the ttf res Length %v less than %v", len(txg.res.Ttf), latest)
	}
	return txg.res.Ttf[len(txg.res.Ttf)-1-int(latest) : len(txg.res.Ttf)-1], nil
}

func (txg *TxGenAPI) GetTotalSend() uint64 {
	return txg.res.TotalTxSend
}

func (txg *TxGenAPI) DeployContracts(prikey string, configPath string) error {
	return handelTxGenConfig(configPath, func(txgenInput *TxGenInput) error {
		pri, err := crypto.HexToECDSA(prikey)
		if err != nil {
			return err
		}
		currentState, err := txg.eth.blockchain.State()
		if err != nil {
			return err
		}
		defer currentState.ClearReference()
		account := crypto.PubkeyToAddress(pri.PublicKey)
		nonce := currentState.GetNonce(account)
		singine := types.NewEIP155Signer(new(big.Int).SetInt64(txg.eth.chainConfig.ChainID.Int64()))
		gasPrice := txg.eth.txPool.GasPrice()

		for _, input := range [][]*TxGenContractConfig{txgenInput.Wasm, txgenInput.Evm} {
			for _, config := range input {
				if config.CallWeights == 0 {
					continue
				}
				tx := types.NewContractCreation(nonce, nil, config.DeployGasLimit, gasPrice, common.Hex2Bytes(config.ContractsCode))
				newTx, err := types.SignTx(tx, singine, pri)
				if err != nil {
					return err
				}
				if err := txg.eth.TxPool().AddRemote(newTx); err != nil {
					return fmt.Errorf("DeployContracts fail,err:%v,input:%v", err, config.Type)
				}
				config.DeployTxHash = newTx.Hash().String()
				nonce++
			}
		}
		return nil
	})
}

func (txg *TxGenAPI) UpdateConfig(configPath string) error {
	return handelTxGenConfig(configPath, func(txgenInput *TxGenInput) error {
		for _, input := range [][]*TxGenContractConfig{txgenInput.Wasm, txgenInput.Evm} {
			for _, config := range input {
				if config.CallWeights == 0 {
					continue
				}
				hash := common.HexToHash(config.DeployTxHash)
				tx, blockHash, _, index := rawdb.ReadTransaction(txg.eth.ChainDb(), hash)
				if tx == nil {
					return fmt.Errorf("the tx not find yet,tx:%s", hash.String())

				}
				receipts := txg.eth.blockchain.GetReceiptsByHash(blockHash)
				if len(receipts) <= int(index) {
					return fmt.Errorf("the tx receipts not find yet,tx:%s", hash.String())
				}
				receipt := receipts[index]
				if receipt.Status == 0 {
					return fmt.Errorf("the tx receipts status is 0 ,tx:%s", hash.String())
				}
				config.ContractsAddress = receipt.ContractAddress.String()
			}

		}
		return nil
	})
}

func handelTxGenConfig(configPath string, handle func(*TxGenInput) error) error {
	file, err := os.OpenFile(configPath, os.O_RDWR, 0666)
	if err != nil {
		return fmt.Errorf("Failed to open config file:%v", err)
	}
	defer file.Close()
	var txgenInput TxGenInput
	if err := json.NewDecoder(file).Decode(&txgenInput); err != nil {
		return fmt.Errorf("invalid TxGenConfig file r:%v", err)
	}

	if err := handle(&txgenInput); err != nil {
		return err
	}

	output, err := json.MarshalIndent(txgenInput, "", "    ")
	if err != nil {
		return err
	}

	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := file.Write(output); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

func (txg *TxGenAPI) Stop(resPath string) error {
	if !txg.start {
		return errors.New("the tx maker has been closed")
	}
	if resPath == "" {
		close(txg.txGenStopTxCh)
		close(txg.txGenExitCh)
		txg.start = false
		txg.blockfeed.Unsubscribe()
		txg.blockExecuteFeed.Unsubscribe()
		atomic.StoreUint32(&txg.eth.protocolManager.acceptRemoteTxs, 0)
		return nil
	}
	file, err := os.Create(resPath)
	if err != nil {
		return fmt.Errorf("Failed to open config file:%v", err)
	}
	defer file.Close()

	close(txg.txGenStopTxCh)
	time.Sleep(time.Second * 10)

	output, err := json.Marshal(txg.res)
	if err != nil {
		return err
	}

	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := file.Write(output); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}

	close(txg.txGenExitCh)
	txg.start = false
	txg.blockfeed.Unsubscribe()
	txg.blockExecuteFeed.Unsubscribe()

	atomic.StoreUint32(&txg.eth.protocolManager.acceptRemoteTxs, 0)

	return nil
}

type TxGenInput struct {
	Wasm []*TxGenContractConfig     `json:"wasm"`
	Evm  []*TxGenContractConfig     `json:"evm"`
	Tx   []*TxGenInputAccountConfig `json:"tx"`
}

type TxGenInputAccountConfig struct {
	Pri string `json:"private_key"`
	Add string `json:"address"`
}

type TxGenContractConfig struct {
	//CreateContracts
	DeployTxHash     string `json:"deploy_contract_tx_hash"`
	DeployGasLimit   uint64 `json:"deploy_gas_limit"`
	Type             string `json:"contracts_type"`
	Name             string `json:"name"`
	ContractsCode    string `json:"contracts_code"`
	ContractsAddress string `json:"contracts_address"`

	//CallContracts
	CallWeights uint                 `json:"call_weights"`
	CallKind    uint                 `json:"call_kind"`
	CallConfig  []ContractCallConfig `json:"call_config"`
}

type ContractCallConfig struct {
	GasLimit   uint64        `json:"call_gas_limit"`
	Input      string        `json:"call_input"`
	Parameters []interface{} `json:"parameters"`
}

type txGenSendAccount struct {
	Priv    *ecdsa.PrivateKey
	Nonce   uint64
	Address common.Address

	ReceiptsNonce uint64
	LastSendTime  time.Time
	SendTime      map[uint64]time.Time
}

func (account *txGenSendAccount) active(waitAccountReceiptTime time.Duration) bool {
	if account.Nonce >= account.ReceiptsNonce+10 {
		waitTime := time.Since(account.LastSendTime)
		if waitTime >= waitAccountReceiptTime {
			log.Debug("wait account too much time", "account", account.Address, "nonce", account.Nonce, "receiptnonce", account.ReceiptsNonce, "wait time", waitTime)
			account.Nonce = account.ReceiptsNonce + 1
			return true
		}
		return false
	}
	if account.ReceiptsNonce > account.Nonce {
		account.Nonce = account.ReceiptsNonce + 1
	}
	return true
}

const (
	callKindDefine   = 0
	callKindGenerate = 1
)

type txGenContractReceiver struct {
	ContractsAddress common.Address
	Weights          uint
	CallInputs       []ContractReceiverCallInput
	CallKind         uint

	Type string
}

func (t *txGenContractReceiver) pickCallInput() ContractReceiverCallInput {
	if len(t.CallInputs) == 1 {
		return t.CallInputs[0]
	}
	return t.CallInputs[rand.Intn(len(t.CallInputs))]
}

type ContractReceiverCallInput struct {
	Data       []byte
	GasLimit   uint64
	Parameters []interface{}
}

func newAccountQueue(accounts []common.Address) *accountQueue {
	queue := new(accountQueue)
	queue.accounts = accounts
	queue.length = len(accounts)
	queue.current = len(accounts)
	return queue
}

type accountQueue struct {
	accounts []common.Address
	current  int
	length   int
}

func (a *accountQueue) next() common.Address {
	a.current++
	if a.current >= a.length {
		a.current = 0
	}
	return a.accounts[a.current]
}

type TxMakeManger struct {
	//from
	accounts map[common.Address]*txGenSendAccount

	blockCache map[common.Hash]*types.Block

	activeSender      *accountQueue
	activeSenderTxPer int
	normalSender      *accountQueue
	totalSenderTxPer  int
	txFrequency       int
	amount            *big.Int

	//to
	txReceiver   []common.Address
	evmReceiver  weightedrand.Chooser
	wsamReveiver weightedrand.Chooser

	blockProduceTime time.Time

	sendTx   uint
	sendEvm  uint
	sendWasm uint

	sendState uint
}

func (s *TxMakeManger) pickActiveSender() *txGenSendAccount {
	return s.accounts[s.activeSender.next()]
}

func (s *TxMakeManger) pickNormalSender() *txGenSendAccount {
	return s.accounts[s.normalSender.next()]
}

func (s *TxMakeManger) pickTxReceive() common.Address {
	return s.txReceiver[rand.Intn(len(s.txReceiver))]
}

var (
	evmErc20Hash = func() []byte {
		prifix := sha3.NewKeccak256()
		prifix.Write([]byte("transfer(address,uint256)"))
		return prifix.Sum(nil)
	}()

	evmKVHash = func() []byte {
		prifix := sha3.NewKeccak256()
		prifix.Write([]byte("SetKV(uint256,uint256)"))
		return prifix.Sum(nil)
	}()

	evmKVHashAddr = func() []byte {
		prifix := sha3.NewKeccak256()
		prifix.Write([]byte("SetKV(uint256)"))
		return prifix.Sum(nil)
	}()

	wasmErc20Hash = func() []byte {
		hash := fnv.New64()
		hash.Write([]byte("transfer"))
		return hash.Sum(nil)
	}()
	wasmkVHash = func() []byte {
		hash := fnv.New64()
		hash.Write([]byte("setKey"))
		return hash.Sum(nil)
	}()
)

type WasmERC20Info struct {
	Method  []byte
	Address common.Address
	Amount  uint64
}

type WasmKeyValueInfo struct {
	Method []byte
	Key    uint32
	Count  uint32
}

type WasmKeyValueAddrInfo struct {
	Method []byte
	Val    uint32
}

var one = common.Uint16ToBytes(1)

func (s *TxMakeManger) generateTxParams(add common.Address) ([]byte, common.Address, uint64, *big.Int) {
	switch {
	case s.sendState < s.sendTx:
		return nil, add, 21000, s.amount
	case s.sendState < s.sendEvm:
		account := s.evmReceiver.Pick().(*txGenContractReceiver)
		if account.CallKind == callKindDefine {
			input := account.pickCallInput()
			return input.Data, account.ContractsAddress, input.GasLimit, nil
		} else {
			if account.Type == "erc20" {
				return BuildEVMInput(evmErc20Hash, add.Bytes(), one), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			} else if account.Type == "kv" {
				key, count := int32(account.CallInputs[0].Parameters[0].(float64)), uint32(account.CallInputs[0].Parameters[1].(float64))
				return BuildEVMInput(evmKVHash, common.Uint32ToBytes(uint32(rand.Int31n(key))), common.Uint32ToBytes(count)), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			} else if account.Type == "kv_addr" {
				val := int32(account.CallInputs[0].Parameters[0].(float64))
				return BuildEVMInput(evmKVHashAddr, common.Uint32ToBytes(uint32(rand.Int31n(val)))), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			}
		}
	case s.sendState < s.sendWasm:
		account := s.wsamReveiver.Pick().(*txGenContractReceiver)
		if account.CallKind == callKindDefine {
			input := account.pickCallInput()
			return input.Data, account.ContractsAddress, input.GasLimit, nil
		} else {
			if account.Type == "erc20" {
				return BuildWASMInput(WasmERC20Info{wasmErc20Hash, add, 1}), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			} else if account.Type == "kv" {
				key, count := int32(account.CallInputs[0].Parameters[0].(float64)), uint32(account.CallInputs[0].Parameters[1].(float64))
				return BuildWASMInput(WasmKeyValueInfo{wasmkVHash, uint32(rand.Int31n(key)), count}), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			} else if account.Type == "kv_addr" {
				val := int32(account.CallInputs[0].Parameters[0].(float64))
				return BuildWASMInput(WasmKeyValueAddrInfo{wasmkVHash, uint32(rand.Int31n(val))}), account.ContractsAddress, account.CallInputs[0].GasLimit, nil
			}
		}
	}
	log.Crit("generateTxParams fail,the sendState should not grate than the sendWasm", "state", s.sendState, "wasm", s.sendWasm)
	return nil, common.Address{}, 0, nil
}

func (s *TxMakeManger) sendDone(account *txGenSendAccount, now time.Time) {
	s.sendState++
	if s.sendState >= s.sendWasm {
		s.sendState = 0
	}

	account.SendTime[account.Nonce] = now
	account.LastSendTime = now
	account.Nonce = account.Nonce + 1
}

func NewTxMakeManger(tx, evm, wasm uint, totalTxPer, activeTxPer, txFrequency, activeSender uint, sendingAmount uint64, GetNonce func(addr common.Address) uint64, getCodeSize func(addr common.Address) int, accountPath string, start, end uint) (*TxMakeManger, error) {
	if end-start+1 < activeSender {
		return nil, fmt.Errorf("the active sender can't more than total account,total:%v,active:%v", end-start+1, activeSender)
	}

	file, err := os.Open(accountPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to read genesis file:%v", err)
	}
	defer file.Close()

	var txgenInput TxGenInput
	if err := json.NewDecoder(file).Decode(&txgenInput); err != nil {
		return nil, fmt.Errorf("invalid genesis file chain id:%v", err)
	}

	t := new(TxMakeManger)
	t.amount = new(big.Int).SetUint64(sendingAmount)
	t.accounts = make(map[common.Address]*txGenSendAccount)
	t.txReceiver = make([]common.Address, 0)
	active := make([]common.Address, 0, activeSender)
	nomral := make([]common.Address, 0, end-start+1-activeSender)

	currentAccountLenth := uint(0)
	for i := start; i <= end; i++ {
		privateKey, err := crypto.HexToECDSA(txgenInput.Tx[i].Pri)
		if err != nil {
			return nil, fmt.Errorf("NewTxMakeManger HexToECDSA fail:%v", err)
		}
		address, err := common.Bech32ToAddress(txgenInput.Tx[i].Add)
		if err != nil {
			return nil, fmt.Errorf("NewTxMakeManger Bech32ToAddress fail:%v", err)
		}
		nonce := GetNonce(address)
		now := time.Now()
		t.accounts[address] = &txGenSendAccount{privateKey, nonce, address, nonce, now, nil}
		t.accounts[address].SendTime = make(map[uint64]time.Time)
		t.accounts[address].SendTime[nonce] = now
		t.txReceiver = append(t.txReceiver, address)

		if currentAccountLenth < activeSender {
			active = append(active, address)
		} else {
			nomral = append(nomral, address)
		}
		currentAccountLenth++
	}
	t.normalSender = newAccountQueue(nomral)
	t.activeSender = newAccountQueue(active)
	t.totalSenderTxPer = int(totalTxPer)
	t.activeSenderTxPer = int(activeTxPer)
	t.txFrequency = int(txFrequency)

	t.blockCache = make(map[common.Hash]*types.Block)

	t.blockProduceTime = time.Now()

	rand.Seed(time.Now().UTC().UnixNano()) // always seed random!

	t.sendTx = tx
	t.sendEvm = tx + evm
	t.sendWasm = tx + evm + wasm
	if t.sendWasm == 0 {
		return nil, errors.New("new tx gen fail ,tx+evm+wasm size should not be zero")
	}
	if evm+wasm == 0 {
		return t, nil
	}

	if evm > 0 && len(txgenInput.Evm) == 0 {
		return nil, errors.New("new tx gen fail ,evm config not set")
	}
	if wasm > 0 && len(txgenInput.Wasm) == 0 {
		return nil, errors.New("new tx gen fail ,wasm config not set")
	}

	evmChooser := make([]weightedrand.Choice, 0, len(txgenInput.Evm))

	wasmChooser := make([]weightedrand.Choice, 0, len(txgenInput.Wasm))

	for i, ContractConfigs := range [][]*TxGenContractConfig{txgenInput.Evm, txgenInput.Wasm} {
		for _, config := range ContractConfigs {
			if config.CallWeights != 0 {
				txReceiver := new(txGenContractReceiver)
				txReceiver.ContractsAddress = common.MustBech32ToAddress(config.ContractsAddress)
				if getCodeSize(txReceiver.ContractsAddress) <= 0 {
					return nil, fmt.Errorf("new tx gen fail the address don't have code,add:%s", txReceiver.ContractsAddress.String())
				}
				txReceiver.CallKind = config.CallKind
				txReceiver.CallInputs = make([]ContractReceiverCallInput, 0)
				for _, config := range config.CallConfig {
					if txReceiver.CallKind == callKindDefine && config.Input == "" {
						return nil, fmt.Errorf("NewTxMakeManger  fail:the call_input can't be nil if CallKind is 0")
					}
					txReceiver.CallInputs = append(txReceiver.CallInputs, ContractReceiverCallInput{
						Data:       common.Hex2Bytes(config.Input),
						GasLimit:   config.GasLimit,
						Parameters: config.Parameters,
					})
				}
				txReceiver.Weights = config.CallWeights
				txReceiver.Type = config.Type
				if i == 0 {
					evmChooser = append(evmChooser, weightedrand.NewChoice(txReceiver, txReceiver.Weights))
				} else {
					wasmChooser = append(wasmChooser, weightedrand.NewChoice(txReceiver, txReceiver.Weights))
				}
			}
		}
	}
	t.evmReceiver = weightedrand.NewChooser(evmChooser...)
	t.wsamReveiver = weightedrand.NewChooser(wasmChooser...)

	return t, nil
}

func BuildEVMInput(funcName []byte, params ...[]byte) []byte {
	input := make([]byte, 4+32*len(params))

	copy(input[:4], funcName[:4])

	for i, param := range params {
		copy(input[4+32*(i+1)-len(param):4+32*(i+1)], param)
	}
	return input
}

func BuildWASMInput(rawStruct interface{}) []byte {
	rlpev, _ := rlp.EncodeToBytes(rawStruct)
	return rlpev
}
