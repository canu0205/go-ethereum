// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Implements interaction with EVM1Interpreter-based VMs.
// https://github.com/ethereum/evmc

package vm

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ethereum/evmc/v12/bindings/go/evmc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// EVM1Interpreter represents the reference to a common EVM1Interpreter-based VM instance and
// the current execution context as required by go-ethereum design.
type EVM1Interpreter struct {
	instance *evmc.VM        // The reference to the EVM1Interpreter VM instance.
	cap      evmc.Capability // The supported EVM1Interpreter capability (EVM or Ewasm)

	evm *EVM // The execution context.
	//table *JumpTable

	//hasher    crypto.KeccakState // Keccak256 hasher instance shared across opcodes
	//hasherBuf common.Hash        // Keccak256 hasher result array shared across opcodes

	readOnly   bool   // The readOnly flag (TODO: Try to get rid of it).
	returnData []byte // Last CALL's return data for subsequent reuse
}

var (
	evmModule   *evmc.VM
	ewasmModule *evmc.VM
)

func InitEVM1EVM(config string) {
	evmModule = initEVM1(evmc.CapabilityEVM1, config)
}

func InitEVMCEwasm(config string) {
	ewasmModule = initEVM1(evmc.CapabilityEWASM, config)
}

func initEVM1(cap evmc.Capability, config string) *evmc.VM {
	options := strings.Split(config, ",")
	path := options[0]

	if path == "" {
		panic("EVM1Interpreter VM path not provided, set --vm.(evm|ewasm)=/path/to/vm")
	}

	instance, err := evmc.Load(path)
	if err != nil {
		panic(err.Error())
	}
	log.Info("EVM1Interpreter VM loaded", "name", instance.Name(), "version", instance.Version(), "path", path)

	// Set options before checking capabilities.
	for _, option := range options[1:] {
		if idx := strings.Index(option, "="); idx >= 0 {
			name := option[:idx]
			value := option[idx+1:]
			err := instance.SetOption(name, value)
			if err == nil {
				log.Info("EVM1Interpreter VM option set", "name", name, "value", value)
			} else {
				log.Warn("EVM1Interpreter VM option setting failed", "name", name, "error", err)
			}
		}
	}

	if !instance.HasCapability(cap) {
		panic(fmt.Errorf("The EVM1Interpreter module %s does not have requested capability %d", path, cap))
	}
	return instance
}

// hostContext implements evmc.HostContext interface.
type hostContext struct {
	evm      *EVM      // The reference to the EVM execution context.
	contract *Contract // The reference to the current contract, needed by Call-like methods.
}

func (host *hostContext) AccountExists(addr evmc.Address) bool {
	if host.evm.ChainConfig().IsEIP158(host.evm.Context.BlockNumber) {
		if !host.evm.StateDB.Empty(common.Address(addr)) {
			return true
		}
	} else if host.evm.StateDB.Exist(common.Address(addr)) {
		return true
	}
	return false
}

func (host *hostContext) GetStorage(addr evmc.Address, key evmc.Hash) evmc.Hash {
	return evmc.Hash(host.evm.StateDB.GetState(common.Address(addr), common.Hash(key)))
}

func (host *hostContext) SetStorage(addr_ evmc.Address, key_ evmc.Hash, value_ evmc.Hash) (status evmc.StorageStatus) {
	addr := common.Address(addr_)
	key := common.Hash(key_)
	value := common.Hash(value_)
	oldValue := host.evm.StateDB.GetState(addr, key)
	if oldValue == value {
		return evmc.StorageAssigned
	}

	current := host.evm.StateDB.GetState(addr, key)
	original := host.evm.StateDB.GetCommittedState(addr, key)

	host.evm.StateDB.SetState(addr, key, value)

	hasEIP2200 := host.evm.ChainConfig().IsIstanbul(host.evm.Context.BlockNumber)
	hasNetStorageCostEIP := hasEIP2200 ||
		(host.evm.ChainConfig().IsConstantinople(host.evm.Context.BlockNumber) &&
			!host.evm.ChainConfig().IsPetersburg(host.evm.Context.BlockNumber))
	if !hasNetStorageCostEIP {
		zero := common.Hash{}
		if oldValue == zero {
			return evmc.StorageAdded
		} else if value == zero {
			host.evm.StateDB.AddRefund(params.SstoreRefundGas)
			return evmc.StorageDeleted
		}
		return evmc.StorageModified
	}

	resetClearRefund := params.NetSstoreResetClearRefund
	cleanRefund := params.NetSstoreResetRefund

	if hasEIP2200 {
		resetClearRefund = params.SstoreSetGasEIP2200 - params.SloadGasEIP2200
		cleanRefund = params.SstoreResetGasEIP2200 - params.SloadGasEIP2200
	}

	if original == current {
		if original == (common.Hash{}) { // create slot (2.1.1)
			return evmc.StorageAdded
		}
		if value == (common.Hash{}) { // delete slot (2.1.2b)
			host.evm.StateDB.AddRefund(params.NetSstoreClearRefund)
			return evmc.StorageDeleted
		}
		return evmc.StorageModified
	}
	if original != (common.Hash{}) {
		if current == (common.Hash{}) { // recreate slot (2.2.1.1)
			host.evm.StateDB.SubRefund(params.NetSstoreClearRefund)
		} else if value == (common.Hash{}) { // delete slot (2.2.1.2)
			host.evm.StateDB.AddRefund(params.NetSstoreClearRefund)
		}
	}
	if original == value {
		if original == (common.Hash{}) { // reset to original inexistent slot (2.2.2.1)
			host.evm.StateDB.AddRefund(resetClearRefund)
		} else { // reset to original existing slot (2.2.2.2)
			host.evm.StateDB.AddRefund(cleanRefund)
		}
	}
	return evmc.StorageModifiedRestored
}

func (host *hostContext) GetBalance(addr evmc.Address) evmc.Hash {
	balance := host.evm.StateDB.GetBalance(common.Address(addr))
	return evmc.Hash(common.BigToHash(balance.ToBig()))
}

func (host *hostContext) GetCodeSize(addr evmc.Address) int {
	return host.evm.StateDB.GetCodeSize(common.Address(addr))
}

func (host *hostContext) GetCodeHash(addr evmc.Address) evmc.Hash {
	if host.evm.StateDB.Empty(common.Address(addr)) {
		return evmc.Hash{}
	}
	return evmc.Hash(host.evm.StateDB.GetCodeHash(common.Address(addr)))
}

func (host *hostContext) GetCode(addr evmc.Address) []byte {
	return host.evm.StateDB.GetCode(common.Address(addr))
}

func (host *hostContext) Selfdestruct(addr_ evmc.Address, beneficiary evmc.Address) bool {
	addr := common.Address(addr_)
	db := host.evm.StateDB

	// Check if already self-destructed
	if db.HasSelfDestructed(addr) {
		return false
	}

	// Add refund if not already self-destructed
	db.AddRefund(params.SelfdestructRefundGas)

	// Transfer balance to beneficiary
	balance := db.GetBalance(addr)
	db.AddBalance(common.Address(beneficiary), balance, tracing.BalanceIncreaseSelfdestruct)

	// Mark as self-destructed
	db.SelfDestruct(addr)
	return true
}

func (host *hostContext) GetTxContext() evmc.TxContext {
	return evmc.TxContext{
		GasPrice:    evmc.Hash(common.BigToHash(host.evm.TxContext.GasPrice)),
		Origin:      evmc.Address(host.evm.TxContext.Origin),
		Coinbase:    evmc.Address(host.evm.Context.Coinbase),
		Number:      host.evm.Context.BlockNumber.Int64(),
		Timestamp:   int64(host.evm.Context.Time),
		GasLimit:    int64(host.evm.Context.GasLimit),
		PrevRandao:  evmc.Hash(*host.evm.Context.Random),
		ChainID:     evmc.Hash(common.BigToHash(host.evm.ChainConfig().ChainID)),
		BaseFee:     evmc.Hash(common.BigToHash(host.evm.Context.BaseFee)),
		BlobBaseFee: evmc.Hash(common.BigToHash(host.evm.Context.BlobBaseFee)),
	}
}

func (host *hostContext) GetBlockHash(number int64) evmc.Hash {
	b := host.evm.Context.BlockNumber.Int64()
	if number >= (b-256) && number < b {
		return evmc.Hash(host.evm.Context.GetHash(uint64(number)))
	}
	return evmc.Hash{}
}

func (host *hostContext) EmitLog(addr evmc.Address, topics_ []evmc.Hash, data []byte) {
	topics := make([]common.Hash, len(topics_))
	for i := 0; i < len(topics); i++ {
		topics[i] = common.Hash(topics_[i])
	}

	host.evm.StateDB.AddLog(&types.Log{
		Address:     common.Address(addr),
		Topics:      topics,
		Data:        data,
		BlockNumber: host.evm.Context.BlockNumber.Uint64(),
	})
}

func (host *hostContext) Call(kind evmc.CallKind,
	recipient_ evmc.Address, sender_ evmc.Address, value_ evmc.Hash, input []byte, gas int64, depth int,
	static bool, salt evmc.Hash, codeAddress_ evmc.Address) (output []byte, gasLeft int64, gasRefund int64, createAddr_ evmc.Address, err error) {

	recipient := common.Address(recipient_)
	sender := common.Address(sender_)
	codeAddress := common.Address(codeAddress_)
	value := uint256.MustFromBig(common.Hash(value_).Big())
	gasU := uint64(gas)
	var gasLeftU uint64
	var createAddr common.Address

	// Track initial refund to calculate gasRefund
	initialRefund := host.evm.StateDB.GetRefund()

	switch kind {
	case evmc.Call:
		if static {
			output, gasLeftU, err = host.evm.StaticCall(sender, recipient, input, gasU)
		} else {
			output, gasLeftU, err = host.evm.Call(sender, recipient, input, gasU, value)
		}
	case evmc.DelegateCall:
		output, gasLeftU, err = host.evm.DelegateCall(host.contract.Caller(), sender, recipient, input, gasU, host.contract.Value())
	case evmc.CallCode:
		output, gasLeftU, err = host.evm.CallCode(sender, recipient, input, gasU, value)
	case evmc.Create:
		var createOutput []byte
		createOutput, createAddr, gasLeftU, err = host.evm.Create(sender, input, gasU, value)
		isHomestead := host.evm.ChainConfig().IsHomestead(host.evm.Context.BlockNumber)
		if !isHomestead && err == ErrCodeStoreOutOfGas {
			err = nil
		}
		if err == ErrExecutionReverted {
			// Assign return buffer from REVERT.
			// TODO: Bad API design: return data buffer and the code is returned in the same place. In worst case
			//       the code is returned also when there is not enough funds to deploy the code.
			output = createOutput
		}
	case evmc.Create2:
		var createOutput []byte
		createOutput, createAddr, gasLeftU, err = host.evm.Create2(sender, input, gasU, value, uint256.MustFromBig(common.Hash(salt).Big()))
		if err == ErrExecutionReverted {
			// Assign return buffer from REVERT.
			// TODO: Bad API design: return data buffer and the code is returned in the same place. In worst case
			//       the code is returned also when there is not enough funds to deploy the code.
			output = createOutput
		}
	default:
		panic(fmt.Errorf("EVM1Interpreter: Unknown call kind %d", kind))
	}

	// Calculate gas refund (difference from initial refund)
	gasRefund = int64(host.evm.StateDB.GetRefund() - initialRefund)

	// Map errors.
	if err == ErrExecutionReverted {
		err = evmc.Revert
	} else if err != nil {
		err = evmc.Failure
	}

	gasLeft = int64(gasLeftU)

	// Note: codeAddress and depth parameters are used in some advanced scenarios
	// but for basic EVMC integration, they're handled by the go-ethereum EVM internally
	_ = codeAddress // Mark as used to avoid compiler warning
	_ = depth       // Mark as used to avoid compiler warning

	return output, gasLeft, gasRefund, evmc.Address(createAddr), err
}

func (host *hostContext) AccessAccount(addr evmc.Address) evmc.AccessStatus {
	if host.evm.StateDB.AddressInAccessList(common.Address(addr)) {
		return evmc.WarmAccess
	}
	host.evm.StateDB.AddAddressToAccessList(common.Address(addr))
	return evmc.ColdAccess
}

func (host *hostContext) AccessStorage(addr evmc.Address, key evmc.Hash) evmc.AccessStatus {
	_, slotPresent := host.evm.StateDB.SlotInAccessList(common.Address(addr), common.Hash(key))
	if slotPresent {
		return evmc.WarmAccess
	}
	host.evm.StateDB.AddSlotToAccessList(common.Address(addr), common.Hash(key))
	return evmc.ColdAccess
}

func (host *hostContext) GetTransientStorage(addr evmc.Address, key evmc.Hash) evmc.Hash {
	return evmc.Hash(host.evm.StateDB.GetTransientState(common.Address(addr), common.Hash(key)))
}

func (host *hostContext) SetTransientStorage(addr evmc.Address, key evmc.Hash, value evmc.Hash) {
	host.evm.StateDB.SetTransientState(common.Address(addr), common.Hash(key), common.Hash(value))
}

// getRevision translates ChainConfig's HF block information into EVM1Interpreter revision.
func getRevision(evm *EVM) evmc.Revision {
	n := evm.Context.BlockNumber
	t := evm.Context.Time
	conf := evm.ChainConfig()
	switch {
	case conf.IsCancun(n, t):
		return evmc.Cancun
	case conf.IsShanghai(n, t):
		return evmc.Shanghai
	case evm.chainRules.IsMerge:
		return evmc.Paris
	case conf.IsLondon(n):
		return evmc.London
	case conf.IsBerlin(n):
		return evmc.Berlin
	case conf.IsIstanbul(n):
		return evmc.Istanbul
	case conf.IsPetersburg(n):
		return evmc.Petersburg
	case conf.IsConstantinople(n):
		return evmc.Constantinople
	case conf.IsByzantium(n):
		return evmc.Byzantium
	case conf.IsEIP158(n):
		return evmc.SpuriousDragon
	case conf.IsEIP150(n):
		return evmc.TangerineWhistle
	case conf.IsHomestead(n):
		return evmc.Homestead
	default:
		return evmc.Frontier
	}
}

// Run implements Interpreter.Run().
func (in1 *EVM1Interpreter) Run(contract *Contract, input []byte, readOnly bool) (ret []byte, err error) {
	in1.evm.depth++
	defer func() { in1.evm.depth-- }()

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	kind := evmc.Call
	if in1.evm.StateDB.GetCodeSize(contract.Address()) == 0 {
		// Guess if this is a CREATE.
		kind = evmc.Create
	}

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This makes also sure that the readOnly flag isn't removed for child calls.
	if readOnly && !in1.readOnly {
		in1.readOnly = true
		defer func() { in1.readOnly = false }()
	}

	res, err := in1.instance.Execute(
		&hostContext{in1.evm, contract},
		getRevision(in1.evm),
		kind,
		in1.readOnly,
		in1.readOnly, // TODO: change it to delegated bool
		in1.evm.depth-1,
		int64(contract.Gas),
		evmc.Address(contract.Address()),
		evmc.Address(contract.Caller()),
		input,
		evmc.Hash(common.BigToHash(contract.Value().ToBig())),
		contract.Code,
	)

	ret = res.Output
	gasLeft, gasRefund := res.GasLeft, res.GasRefund
	contract.Gas = uint64(gasLeft)

	// Apply gas refund from EVMC execution
	if gasRefund > 0 {
		in1.evm.StateDB.AddRefund(uint64(gasRefund))
	}

	if err == evmc.Revert {
		err = ErrExecutionReverted
	} else if evmcError, ok := err.(evmc.Error); ok && evmcError.IsInternalError() {
		panic(fmt.Sprintf("EVM1Interpreter VM internal error: %s", evmcError.Error()))
	}

	return ret, err
}

// CanRun implements Interpreter.CanRun().
func (in1 *EVM1Interpreter) CanRun(code []byte) bool {
	required := evmc.CapabilityEVM1
	wasmPreamble := []byte("\x00asm")
	if bytes.HasPrefix(code, wasmPreamble) {
		required = evmc.CapabilityEWASM
	}
	return in1.cap == required
}

// NewEVM1Interpreter creates a new EVM1Interpreter interpreter instance.
func NewEVM1Interpreter(evm *EVM) *EVM1Interpreter {
	if evmModule == nil {
		// Fall back to the default interpreter if EVM1Interpreter is not initialized
		return nil
	}
	return &EVM1Interpreter{
		instance: evmModule,
		cap:      evmc.CapabilityEVM1,
		evm:      evm,
	}
}

// EVM returns the EVM instance
func (in1 *EVM1Interpreter) EVM() *EVM {
	return in1.evm
}

// Config returns the configuration of the interpreter
func (in1 EVM1Interpreter) Config() Config {
	return in1.evm.Config
}

// ReadOnly returns whether the interpreter is in read-only mode
func (in1 EVM1Interpreter) ReadOnly() bool {
	return in1.readOnly
}

// ReturnData gets the last CALL's return data for subsequent reuse
func (in1 *EVM1Interpreter) ReturnData() []byte {
	return in1.returnData
}

// SetReturnData sets the last CALL's return data
func (in1 *EVM1Interpreter) SetReturnData(data []byte) {
	in1.returnData = data
}
