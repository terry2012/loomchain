package fnConsensus

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"

	dbm "github.com/tendermint/tendermint/libs/db"

	"crypto/rand"
	"crypto/sha512"
)

type SigningThreshold string

const (
	// ChannelIDs need to be unique across all the reactors.
	// so to avoid conflict with other reactor's channel id and
	// Give TM some wiggle room when they add more channel, we are starting
	// channel ids from 0x50 for this reactor.
	FnVoteSetChannel = byte(0x50)
	FnMaj23Channel   = byte(0x51)

	FnProposalInfoSyncChannel = byte(0x52)

	VoteSetIDSize = 32

	StartingNonce int64 = 1

	// Max message size 1 MB
	maxMsgSize = 1000 * 1024

	// To make sure when commit routine executes, voteset shouldnt be modifiable
	CommitRoutineExecutionBufferSeconds = 1
	CommitRoutineExecutionBuffer        = CommitRoutineExecutionBufferSeconds * time.Second

	// Adding the Commit execution buffer to both ProgressInterval and ExpiresIn
	// so that 10 seconds interval
	// is maintained between sync expiration, overall expiration and new proposal

	// ProgressIntervalInSeconds denotes interval (synced across node) between two progress/propose
	ProgressIntervalInSeconds int64 = 60 + CommitRoutineExecutionBufferSeconds

	// FnVoteSet completely expires (for both sync and commit) after this duration
	ExpiresIn = (50 * time.Second) + CommitRoutineExecutionBuffer

	// FnVoteSet cannot be modified beyond this interval
	// but can be used to let behind nodes catch up on nonce
	ExpiresInForSync = 40 * time.Second

	// Max context size 1 KB
	MaxContextSize = 1024

	MaxAllowedTimeDriftInFuture = 10 * time.Second

	BaseProposalDelay = 500 * time.Millisecond

	ProgressLoopStartDelay = 2 * time.Second

	Maj23SigningThreshold SigningThreshold = "Maj23"
	AllSigningThreshold   SigningThreshold = "All"

	ProposalInfoSigningThreshold = Maj23SigningThreshold
)

var ErrInvalidReactorConfiguration = errors.New("invalid reactor configuration")

type OverrideValidator struct {
	Address []byte
	Power   int64
}

type ReactorConfig struct {
	OverrideValidatorSet   []*OverrideValidator
	FnVoteSigningThreshold SigningThreshold
}

func (r *ReactorConfig) IsValid() bool {
	return r == nil || (r.FnVoteSigningThreshold != AllSigningThreshold && r.FnVoteSigningThreshold != Maj23SigningThreshold)
}

type FnConsensusReactor struct {
	p2p.BaseReactor

	connectedPeers map[p2p.ID]p2p.Peer
	state          *ReactorState
	db             dbm.DB
	tmStateDB      dbm.DB
	chainID        string

	fnRegistry FnRegistry

	privValidator types.PrivValidator

	peerMapMtx sync.RWMutex

	stateMtx sync.Mutex

	commitRoutineQuitCh map[string]chan struct{}

	staticValidators *types.ValidatorSet

	cfg *ReactorConfig
}

func NewFnConsensusReactor(chainID string, privValidator types.PrivValidator, fnRegistry FnRegistry, db dbm.DB, tmStateDB dbm.DB, cfg *ReactorConfig) (*FnConsensusReactor, error) {
	if !cfg.IsValid() {
		return nil, ErrInvalidReactorConfiguration
	}

	reactor := &FnConsensusReactor{
		connectedPeers:      make(map[p2p.ID]p2p.Peer),
		db:                  db,
		chainID:             chainID,
		tmStateDB:           tmStateDB,
		fnRegistry:          fnRegistry,
		privValidator:       privValidator,
		commitRoutineQuitCh: make(map[string]chan struct{}),
		cfg:                 cfg,
	}

	reactor.BaseReactor = *p2p.NewBaseReactor("FnConsensusReactor", reactor)
	return reactor, nil
}

func (f *FnConsensusReactor) String() string {
	return "FnConsensusReactor"
}

func (f *FnConsensusReactor) OnStart() error {
	reactorState, err := LoadReactorState(f.db)
	if err != nil {
		return err
	}

	f.stateMtx.Lock()
	defer f.stateMtx.Unlock()

	f.state = reactorState

	fnIDs := f.fnRegistry.GetAll()
	for _, fnID := range fnIDs {
		currentVoteState := f.state.CurrentVoteSets[fnID]
		if currentVoteState != nil {
			if currentVoteState.IsExpired(ExpiresIn) {
				delete(f.state.CurrentVoteSets, fnID)
			}
		}
	}

	if err := SaveReactorState(f.db, f.state, true); err != nil {
		return err
	}

	go f.progressRoutine()
	return nil
}

// GetChannels returns the list of channel descriptors.
func (f *FnConsensusReactor) GetChannels() []*p2p.ChannelDescriptor {
	// Priorities are deliberately set to low, to prevent interfering with core TM
	return []*p2p.ChannelDescriptor{
		{
			ID:                  FnMaj23Channel,
			Priority:            20,
			SendQueueCapacity:   100,
			RecvMessageCapacity: maxMsgSize,
		},
		{
			ID:                  FnVoteSetChannel,
			Priority:            25,
			SendQueueCapacity:   100,
			RecvMessageCapacity: maxMsgSize,
		},
		{
			ID:                  FnProposalInfoSyncChannel,
			Priority:            30,
			SendQueueCapacity:   100,
			RecvMessageCapacity: maxMsgSize,
		},
	}
}

// AddPeer is called by the switch when a new peer is added.
func (f *FnConsensusReactor) AddPeer(peer p2p.Peer) {
	f.peerMapMtx.Lock()
	defer f.peerMapMtx.Unlock()
	f.connectedPeers[peer.ID()] = peer
}

// RemovePeer is called by the switch when the peer is stopped (due to error
// or other reason).
func (f *FnConsensusReactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	f.peerMapMtx.Lock()
	defer f.peerMapMtx.Unlock()
	delete(f.connectedPeers, peer.ID())
}

func (f *FnConsensusReactor) myAddress() []byte {
	return f.privValidator.GetPubKey().Address()
}

func (f *FnConsensusReactor) areWeValidator(currentValidatorSet *types.ValidatorSet) (bool, int) {
	validatorIndex, _ := currentValidatorSet.GetByAddress(f.myAddress())
	return validatorIndex != -1, validatorIndex
}

func (f *FnConsensusReactor) generateVoteSetID() (string, error) {
	randomBytes := make([]byte, VoteSetIDSize)

	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(randomBytes), nil
}

func (f *FnConsensusReactor) calculateMessageHash(message []byte) ([]byte, error) {
	hash := sha512.New()
	_, err := hash.Write(message)
	if err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

func (f *FnConsensusReactor) calculateProgressRoutineSleepTime(areWeValidator bool, ownValidatorIndex int) time.Duration {
	currentEpochTime := time.Now().Unix()
	baseTimeToSleep := ProgressIntervalInSeconds - currentEpochTime%ProgressIntervalInSeconds

	if !areWeValidator {
		return (time.Duration(baseTimeToSleep) * time.Second) + BaseProposalDelay
	}

	return (time.Duration(baseTimeToSleep) * time.Second) + (time.Duration(ownValidatorIndex+1) * BaseProposalDelay)
}

func (f *FnConsensusReactor) initValidatorSet(tmState state.State) error {
	if f.cfg.OverrideValidatorSet == nil {
		return nil
	}

	validatorArray := make([]*types.Validator, 0, len(f.cfg.OverrideValidatorSet))

	for _, overrideValidator := range f.cfg.OverrideValidatorSet {
		validatorIndex, validator := tmState.Validators.GetByAddress(overrideValidator.Address)
		if validatorIndex == -1 {
			return fmt.Errorf("validator specified in override config, doesnt exist in TM validator set")
		}
		validatorArray = append(validatorArray, validator.Copy())
	}

	f.staticValidators = types.NewValidatorSet(validatorArray)

	return nil
}

func (f *FnConsensusReactor) getValidatorSet() *types.ValidatorSet {
	if f.staticValidators == nil {
		tmState := state.LoadState(f.tmStateDB)
		return tmState.Validators
	}

	return f.staticValidators
}

func (f *FnConsensusReactor) getNextProposalInfo(fnID string, state *ReactorState, currentValidatorSet *types.ValidatorSet) *ProposalInfo {
	currentProposalInfo := state.CurrentProposalInfo[fnID]
	nextProposalInfo := &ProposalInfo{}
	lastMaj23VoteSet := state.PreviousMaj23VoteSets[fnID]

	if lastMaj23VoteSet == nil {
		return nil
	}

	if !bytes.Equal(lastMaj23VoteSet.ValidatorsHash, currentValidatorSet.Hash()) {
		nextProposalInfo.LastActiveValidators = make([][]byte, currentValidatorSet.Size())
		nextProposalInfo.CurrentTurn = 0
		currentValidatorSet.Iterate(func(i int, validator *types.Validator) bool {
			nextProposalInfo.LastActiveValidators[i] = validator.Address
			return false
		})

		return nextProposalInfo
	}

	lastMaj23ActiveValidators := lastMaj23VoteSet.ActiveValidators()

	var areActiveValidatorsUnchanged = true

	if currentProposalInfo == nil || (len(lastMaj23ActiveValidators) != len(currentProposalInfo.LastActiveValidators)) {
		areActiveValidatorsUnchanged = false
	} else {
		for i := 0; i < len(lastMaj23ActiveValidators); i++ {
			if !bytes.Equal(lastMaj23ActiveValidators[i], currentProposalInfo.LastActiveValidators[i]) {
				areActiveValidatorsUnchanged = false
				break
			}
		}
	}

	if !areActiveValidatorsUnchanged {
		nextProposalInfo.LastActiveValidators = lastMaj23ActiveValidators
		nextProposalInfo.CurrentTurn = 0
	} else {
		nextProposalInfo.CurrentTurn = currentProposalInfo.CurrentTurn + 1
		if nextProposalInfo.CurrentTurn >= len(nextProposalInfo.LastActiveValidators) {
			nextProposalInfo.CurrentTurn = 0
		}
	}

	return nextProposalInfo
}

func (f *FnConsensusReactor) progressRoutine() {

	var currentState state.State

	// Wait till state is populated
	for currentState = state.LoadState(f.tmStateDB); currentState.IsEmpty(); currentState = state.LoadState(f.tmStateDB) {
		f.Logger.Error("TM state is empty. Cant start progress loop, retrying in some time...")
		time.Sleep(ProgressLoopStartDelay)
	}

	f.initValidatorSet(currentState)

	currentValidators := f.getValidatorSet()

	// Initializing these vars with sane value to calculate initial time
	areWeValidator, ownValidatorIndex := f.areWeValidator(currentValidators)

OUTER_LOOP:
	for {
		// Align to minutes, to make sure this routine runs at almost same time across all nodes
		// Not strictly required
		// state and other variables will be same as the one initialized in second case statement
		timeToSleep := f.calculateProgressRoutineSleepTime(areWeValidator, ownValidatorIndex)

		timer := time.NewTimer(timeToSleep)

		select {
		case <-f.Quit():
			timer.Stop()
			break OUTER_LOOP
		case <-timer.C:
			currentValidators := f.getValidatorSet()
			areWeValidator, ownValidatorIndex = f.areWeValidator(currentValidators)

			f.stateMtx.Lock()

			fnIDs := f.fnRegistry.GetAll()
			sort.Strings(fnIDs)

			fnsEligibleForProposal := make([]string, 0, len(fnIDs))
			correspondingProposalInfo := make(map[string]*ProposalInfo, len(fnIDs))

			for _, fnID := range fnIDs {
				currentVoteState := f.state.CurrentVoteSets[fnID]
				if currentVoteState != nil {
					if currentVoteState.IsExpired(ExpiresIn) {
						f.state.PreviousTimedOutVoteSets[fnID] = f.state.CurrentVoteSets[fnID]
						delete(f.state.CurrentVoteSets, fnID)
						f.Logger.Info("FnConsensusReactor: archiving expired Fn execution", "FnID", fnID)
					} else {
						f.Logger.Info("FnConsensusReactor: unable to propose, previous execution is still pending", "FnID", fnID)
						continue
					}
				}
				fnsEligibleForProposal = append(fnsEligibleForProposal, fnID)
				f.state.CurrentProposalInfo[fnID] = f.getNextProposalInfo(fnID, f.state, currentValidators)
				correspondingProposalInfo[fnID] = f.state.CurrentProposalInfo[fnID]
			}

			if err := SaveReactorState(f.db, f.state, true); err != nil {
				f.Logger.Error("FnConsensusReactor: unable to save reactor state")
				f.stateMtx.Unlock()
				break
			}

			f.stateMtx.Unlock()

			for _, fnID := range fnsEligibleForProposal {
				fn := f.fnRegistry.Get(fnID)
				currentProposalInfo := correspondingProposalInfo[fnID]
				if !areWeValidator || !bytes.Equal(f.myAddress(), currentProposalInfo.CurrentProposer()) {
					continue
				}
				f.propose(fnID, fn, currentValidators, currentProposalInfo, ownValidatorIndex)
			}

		}
	}
}

func (f *FnConsensusReactor) propose(fnID string, fn Fn, currentValidators *types.ValidatorSet, currentProposalInfo *ProposalInfo, validatorIndex int) {
	shouldExecuteFn, ctx, err := fn.PrepareContext()
	if err != nil {
		f.Logger.Error("FnConsensusReactor: received error while executing fn.PrepareContext", "error", err)
		return
	}

	if len(ctx) > MaxContextSize {
		f.Logger.Error("FnConsensusReactor: context cannot be more than", "MaxContextSize", MaxContextSize)
		return
	}

	if !shouldExecuteFn {
		f.Logger.Info("FnConsensusReactor: PrepareContext indicated to not execute fn", "fnID", fnID)
		return
	}

	message, signature, err := fn.GetMessageAndSignature(safeCopyBytes(ctx))
	if err != nil {
		f.Logger.Error("FnConsensusReactor: received error while executing fn.GetMessageAndSignature", "fnID", fnID)
		return
	}

	hash, err := f.calculateMessageHash(message)
	if err != nil {
		f.Logger.Error("FnConsensusReactor: unable to calculate message hash", "fnID", fnID, "error", err)
		return
	}

	if err = fn.MapMessage(safeCopyBytes(ctx), safeCopyBytes(hash), safeCopyBytes(message)); err != nil {
		f.Logger.Error("FnConsensusReactor: received error while executing fn.MapMessage", "fnID", fnID, "error", err)
		return
	}

	executionRequest, err := NewFnExecutionRequest(fnID, f.fnRegistry)
	if err != nil {
		f.Logger.Error("FnConsensusReactor: unable to create Fn execution request as FnID is invalid", "fnID", fnID)
		return
	}

	executionResponse := NewFnExecutionResponse(&FnIndividualExecutionResponse{
		Error:           "",
		Hash:            hash,
		OracleSignature: signature,
		Status:          0,
	}, validatorIndex, currentValidators)

	votesetPayload := NewFnVotePayload(executionRequest, executionResponse)

	f.stateMtx.Lock()

	currentNonce, ok := f.state.CurrentNonces[fnID]
	if !ok {
		currentNonce = 1
	}

	newVoteSetID, err := f.generateVoteSetID()
	if err != nil {
		f.Logger.Error("FnConsensusReactor: unable to generate new vote set id")
		f.stateMtx.Unlock()
		return
	}

	voteSet, err := NewVoteSet(newVoteSetID, currentNonce, f.chainID, ExpiresIn, validatorIndex, ctx,
		votesetPayload, f.privValidator, currentValidators, currentProposalInfo)
	if err != nil {
		f.Logger.Error("FnConsensusReactor: unable to create new voteset", "fnID", fnID, "error", err)
		f.stateMtx.Unlock()
		return
	}

	// It seems we are the only validator, so return the signature and close the case.
	if voteSet.IsAgree(f.cfg.FnVoteSigningThreshold, currentValidators) {
		fn.SubmitMultiSignedMessage(safeCopyBytes(ctx),
			safeCopyBytes(voteSet.Payload.Response.Hash),
			safeCopyDoubleArray(voteSet.Payload.Response.OracleSignatures))
		f.stateMtx.Unlock()
		return
	}

	f.state.CurrentVoteSets[fnID] = voteSet
	quitCh := make(chan struct{})
	f.commitRoutineQuitCh[fnID] = quitCh
	go f.commitRoutine(fnID, time.Unix(voteSet.CreationTime, 0).Add(ExpiresInForSync+CommitRoutineExecutionBuffer), quitCh)

	if err := SaveReactorState(f.db, f.state, true); err != nil {
		f.Logger.Error("FnConsensusReactor: unable to save state", "fnID", fnID, "error", err)
		f.stateMtx.Unlock()
		return
	}

	f.stateMtx.Unlock()

	marshalledBytes, err := voteSet.Marshal()
	if err != nil {
		f.Logger.Error(fmt.Sprintf("FnConsensusReactor: Unable to marshal currentVoteSet at FnID: %s", fnID))
		return
	}

	f.peerMapMtx.RLock()
	for _, peer := range f.connectedPeers {
		peer.Send(FnVoteSetChannel, marshalledBytes)
	}
	f.peerMapMtx.RUnlock()
}

func (f *FnConsensusReactor) handleCommit(fnIDToMonitor string) {
	fn := f.fnRegistry.Get(fnIDToMonitor)
	if fn == nil {
		f.Logger.Error("FnConsensusReactor: fn is nil while trying to access it in commit routine, Ignoring...")
		return
	}

	currentValidators := f.getValidatorSet()

	f.stateMtx.Lock()
	defer f.stateMtx.Unlock()

	areWeValidator, ownValidatorIndex := f.areWeValidator(currentValidators)

	currentVoteSet := f.state.CurrentVoteSets[fnIDToMonitor]
	currentNonce := f.state.CurrentNonces[fnIDToMonitor]

	if !currentVoteSet.IsValid(f.chainID, MaxContextSize, true, ExpiresIn, currentValidators, f.fnRegistry) {
		f.Logger.Error("Invalid VoteSet", "VoteSet", currentVoteSet)
		return
	}

	if !currentVoteSet.HasConverged(f.cfg.FnVoteSigningThreshold, currentValidators) {
		f.Logger.Info("No major 2/3 achived", "VoteSet", currentVoteSet)

		f.state.PreviousTimedOutVoteSets[fnIDToMonitor] = currentVoteSet
		delete(f.state.CurrentVoteSets, fnIDToMonitor)

		previousMaj23VoteSet := f.state.PreviousMaj23VoteSets[fnIDToMonitor]
		if previousMaj23VoteSet != nil {
			marshalledBytes, err := previousMaj23VoteSet.Marshal()
			if err != nil {
				f.Logger.Error("unable to marshal PreviousMaj23VoteSet", "error", err, "fnIDToMonitor", fnIDToMonitor)
				return
			}

			// Propogate your last Maj23, to remedy any issue
			f.peerMapMtx.RLock()
			for _, peer := range f.connectedPeers {
				// TODO: Handle timeout
				peer.Send(FnMaj23Channel, marshalledBytes)
			}
			f.peerMapMtx.RUnlock()
		}
	} else {
		if areWeValidator && currentVoteSet.IsAgree(f.cfg.FnVoteSigningThreshold, currentValidators) {
			numberOfAgreeVotes := currentVoteSet.NumberOfAgreeVotes()

			agreeVoteIndex, err := currentVoteSet.GetAgreeVoteIndexForValidatorIndex(ownValidatorIndex)
			if err != nil {
				f.Logger.Error("FnConsensusReactor: unable to get agree vote index for validator", "validatorIndex", ownValidatorIndex)
				return
			}

			if (currentNonce % int64(numberOfAgreeVotes)) == int64(agreeVoteIndex) {
				fn.SubmitMultiSignedMessage(safeCopyBytes(currentVoteSet.ExecutionContext),
					safeCopyBytes(currentVoteSet.Payload.Response.Hash),
					safeCopyDoubleArray(currentVoteSet.Payload.Response.OracleSignatures))
			}
		}

		f.state.CurrentNonces[fnIDToMonitor]++
		f.state.PreviousValidatorSet = currentValidators
		f.state.PreviousMaj23VoteSets[fnIDToMonitor] = currentVoteSet
		delete(f.state.CurrentVoteSets, fnIDToMonitor)
	}

	if err := SaveReactorState(f.db, f.state, true); err != nil {
		f.Logger.Error("FnConsensusReactor: unable to save state", "fnID", fnIDToMonitor, "error", err)
		return
	}
}

func (f *FnConsensusReactor) commitRoutine(fnIDToMonitor string, monitoringTill time.Time, quitCh <-chan struct{}) {
	unlockDuration := time.Until(monitoringTill)
	timer := time.NewTimer(unlockDuration)

	select {
	case <-quitCh:
		break
	case <-timer.C:
		f.handleCommit(fnIDToMonitor)
		break
	}
}

func (f *FnConsensusReactor) compareVoteSets(remoteVoteSet *FnVoteSet, currentVoteSet *FnVoteSet, currentNonce int64, currentValidators *types.ValidatorSet) int {
	if currentVoteSet == nil {
		if currentNonce == remoteVoteSet.Nonce {
			return 1
		}

		if remoteVoteSet.HasConverged(f.cfg.FnVoteSigningThreshold, currentValidators) {
			return 1
		}

		return -1
	}

	// Perfect candidate to merge
	if currentVoteSet.ID == remoteVoteSet.ID {
		return 0
	}

	currentVoteSetMaj23Agree := currentVoteSet.IsAgree(f.cfg.FnVoteSigningThreshold, currentValidators)
	currentVoteSetMaj23Disagree := currentVoteSet.IsDisagree(f.cfg.FnVoteSigningThreshold, currentValidators)
	currentVoteSetMaj23 := currentVoteSetMaj23Agree || currentVoteSetMaj23Disagree

	remoteVoteSetMaj23Agree := remoteVoteSet.IsAgree(f.cfg.FnVoteSigningThreshold, currentValidators)
	remoteVoteSetMaj23Disagree := remoteVoteSet.IsDisagree(f.cfg.FnVoteSigningThreshold, currentValidators)
	remoteVoteSetMaj23 := remoteVoteSetMaj23Agree || remoteVoteSetMaj23Disagree

	if currentVoteSetMaj23 && !remoteVoteSetMaj23 {
		return -1
	} else if !currentVoteSetMaj23 && remoteVoteSetMaj23 {
		return 1
	} else if !currentVoteSetMaj23 && !remoteVoteSetMaj23 {
		return -1
	}

	if currentVoteSetMaj23Agree && !remoteVoteSetMaj23Agree {
		return -1
	} else if !currentVoteSetMaj23Agree && remoteVoteSetMaj23Agree {
		return 1
	} else if !currentVoteSetMaj23Agree && !remoteVoteSetMaj23Agree {
		return -1
	}

	currentNumberOfVotes := currentVoteSet.NumberOfVotes()
	currentNumberOfAgreeVotes := currentVoteSet.NumberOfAgreeVotes()

	remoteNumberOfVotes := remoteVoteSet.NumberOfVotes()
	remoteNumberOfAgreeVotes := remoteVoteSet.NumberOfAgreeVotes()

	if remoteNumberOfVotes < currentNumberOfVotes {
		return -1
	} else if remoteNumberOfVotes > currentNumberOfVotes {
		return 1
	}

	if remoteNumberOfAgreeVotes < currentNumberOfAgreeVotes {
		return -1
	} else if remoteNumberOfAgreeVotes > currentNumberOfAgreeVotes {
		return 1
	}

	if currentVoteSet.CreationTime > remoteVoteSet.CreationTime {
		return -1
	} else if currentVoteSet.CreationTime < remoteVoteSet.CreationTime {
		return 1
	}

	// If everything is same, we will trust current vote set
	return -1
}

func (f *FnConsensusReactor) handleMaj23VoteSetChannel(sender p2p.Peer, msgBytes []byte) {
	f.stateMtx.Lock()
	defer f.stateMtx.Unlock()

	currentValidatorSet := f.getValidatorSet()
	previousValidatorSet := f.state.PreviousValidatorSet

	validatorSetWhichSignedRemoteVoteSet := currentValidatorSet

	remoteMaj23VoteSet := &FnVoteSet{}
	if err := remoteMaj23VoteSet.Unmarshal(msgBytes); err != nil {
		f.Logger.Error("FnConsensusReactor: Invalid Data passed, ignoring...", "error", err)
		return
	}

	// We might have recently changed validator set, so Maybe this voteset is valid with previousValidatorSet and not current
	if !remoteMaj23VoteSet.IsValid(f.chainID, MaxContextSize, false, ExpiresInForSync, currentValidatorSet, f.fnRegistry) {
		if previousValidatorSet == nil || !remoteMaj23VoteSet.IsValid(f.chainID, MaxContextSize, false, ExpiresInForSync, previousValidatorSet, f.fnRegistry) {
			f.Logger.Error("FnConsensusReactor: Invalid VoteSet specified, ignoring...")
			return
		}
		validatorSetWhichSignedRemoteVoteSet = previousValidatorSet
	}

	remoteFnID := remoteMaj23VoteSet.GetFnID()
	currentNonce, ok := f.state.CurrentNonces[remoteFnID]
	if !ok {
		currentNonce = 1
	}

	currentMaj23VoteSet := f.state.PreviousMaj23VoteSets[remoteFnID]
	needToBroadcast := true

	// This is to enforce that, this voteset must not be active one.
	if !remoteMaj23VoteSet.IsExpired(ExpiresIn) {
		f.Logger.Error("FnConsensusReactor: Got an active voteset, Ignoring...")
		return
	}

	if !remoteMaj23VoteSet.HasConverged(f.cfg.FnVoteSigningThreshold, validatorSetWhichSignedRemoteVoteSet) {
		f.Logger.Error("FnConsensusReactor: got non maj23 voteset, Ignoring...")
		return
	}

	if remoteMaj23VoteSet.Nonce < currentNonce {
		needToBroadcast = false
		if remoteMaj23VoteSet.Nonce == currentNonce-1 {
			if currentMaj23VoteSet == nil {
				currentMaj23VoteSet = remoteMaj23VoteSet
				f.state.PreviousMaj23VoteSets[remoteFnID] = remoteMaj23VoteSet
				f.state.PreviousValidatorSet = validatorSetWhichSignedRemoteVoteSet
				f.state.CurrentProposalInfo[remoteFnID] = remoteMaj23VoteSet.ProposalInfo
			}
		}
	} else {
		// Remote Maj23 is at nonce `x`. So, current nonce must be `x` + 1.
		currentMaj23VoteSet = remoteMaj23VoteSet
		f.state.PreviousMaj23VoteSets[remoteFnID] = remoteMaj23VoteSet
		f.state.PreviousValidatorSet = validatorSetWhichSignedRemoteVoteSet
		f.state.CurrentNonces[remoteFnID] = remoteMaj23VoteSet.Nonce + 1
		f.state.CurrentProposalInfo[remoteFnID] = remoteMaj23VoteSet.ProposalInfo
	}

	if err := SaveReactorState(f.db, f.state, true); err != nil {
		f.Logger.Error("FnConsensusReactor: unable to save reactor state")
		return
	}

	if !needToBroadcast {
		return
	}

	marshalledBytes, err := currentMaj23VoteSet.Marshal()
	if err != nil {
		f.Logger.Error("FnConsensusReactor: unable to marshal bytes")
		return
	}

	f.peerMapMtx.RLock()
	for _, peer := range f.connectedPeers {
		// TODO: Handle timeout
		peer.Send(FnMaj23Channel, marshalledBytes)
	}
	f.peerMapMtx.RUnlock()

}

func (f *FnConsensusReactor) handleVoteSetChannelMessage(sender p2p.Peer, msgBytes []byte) {
	currentValidators := f.getValidatorSet()
	areWeValidator, ownValidatorIndex := f.areWeValidator(currentValidators)

	f.stateMtx.Lock()
	defer f.stateMtx.Unlock()

	remoteVoteSet := &FnVoteSet{}
	if err := remoteVoteSet.Unmarshal(msgBytes); err != nil {
		f.Logger.Error("FnConsensusReactor: Invalid Data passed, ignoring...", "error", err)
		return
	}

	if !remoteVoteSet.IsValid(f.chainID, MaxContextSize, true, ExpiresInForSync, currentValidators, f.fnRegistry) {
		f.Logger.Error("FnConsensusReactor: Invalid VoteSet specified, ignoring...")
		return
	}

	fnID := remoteVoteSet.GetFnID()
	var didWeContribute, hasOurVoteSetChanged bool
	var err error

	currentNonce, ok := f.state.CurrentNonces[remoteVoteSet.GetFnID()]
	if !ok {
		currentNonce = 1
		f.state.CurrentNonces[remoteVoteSet.GetFnID()] = currentNonce
	}
	currentVoteSet := f.state.CurrentVoteSets[remoteVoteSet.GetFnID()]

	if currentNonce != remoteVoteSet.Nonce {
		if currentNonce > remoteVoteSet.Nonce {
			f.Logger.Error("FnConsensusReactor: Already seen this nonce, ignoring", "currentNonce", currentNonce, "remoteNonce", remoteVoteSet.Nonce)
			return
		}
	}

	switch f.compareVoteSets(remoteVoteSet, currentVoteSet, currentNonce, currentValidators) {
	// Both vote set have same trustworthy ness, so merge
	case 0:
		if didWeContribute, err = f.state.CurrentVoteSets[fnID].Merge(currentValidators, remoteVoteSet); err != nil {
			f.Logger.Error("FnConsensusReactor: Unable to merge remote vote set into our own.", "error:", err)
			return
		}
		currentVoteSet = f.state.CurrentVoteSets[fnID]
		currentNonce = f.state.CurrentNonces[fnID]

		hasOurVoteSetChanged = didWeContribute
		break
	// Remote voteset is more trustworthy, so replace
	case 1:
		if currentVoteSet != nil {
			quitCh := f.commitRoutineQuitCh[fnID]
			close(quitCh)
		}

		f.state.CurrentVoteSets[fnID] = remoteVoteSet
		f.state.CurrentNonces[fnID] = remoteVoteSet.Nonce
		f.state.CurrentProposalInfo[fnID] = remoteVoteSet.ProposalInfo

		currentVoteSet = f.state.CurrentVoteSets[fnID]
		currentNonce = f.state.CurrentNonces[fnID]

		hasOurVoteSetChanged = true
		didWeContribute = false

		quitCh := make(chan struct{})
		f.commitRoutineQuitCh[fnID] = quitCh
		go f.commitRoutine(fnID, time.Unix(currentVoteSet.CreationTime, 0).Add(ExpiresInForSync+CommitRoutineExecutionBuffer), quitCh)
		break
	// Current voteset is more trustworthy
	case -1:
		if currentVoteSet == nil {
			return
		}
		break
	}

	if areWeValidator && !currentVoteSet.HaveWeAlreadySigned(ownValidatorIndex) {
		fn := f.fnRegistry.Get(fnID)

		message, signature, err := fn.GetMessageAndSignature(safeCopyBytes(currentVoteSet.ExecutionContext))
		if err != nil {
			f.Logger.Error("FnConsensusReactor: fn.GetMessageAndSignature returned an error, ignoring..")
			return
		}

		hash, err := f.calculateMessageHash(message)
		if err != nil {
			f.Logger.Error("FnConsensusReactor: unable to calculate message hash", "fnID", fnID, "error", err)
			return
		}

		areWeAgreed := (bytes.Compare(currentVoteSet.GetMessageHash(), hash) == 0)

		if err = fn.MapMessage(safeCopyBytes(currentVoteSet.ExecutionContext), safeCopyBytes(hash), safeCopyBytes(message)); err != nil {
			f.Logger.Error("FnConsensusReactor: received error while executing fn.MapMessage", "fnID", fnID, "error", err)
			return
		}

		if areWeAgreed {
			err = currentVoteSet.AddVote(currentNonce, &FnIndividualExecutionResponse{
				Status:          0,
				Error:           "",
				Hash:            hash,
				OracleSignature: signature,
			}, currentValidators, ownValidatorIndex, VoteTypeAgree, f.privValidator)
			if err != nil {
				f.Logger.Error("FnConsensusError: unable to add agree vote to current voteset, ignoring...", "error", err)
				return
			}
		} else {
			err = currentVoteSet.AddVote(currentNonce, &FnIndividualExecutionResponse{
				Status:          0,
				Error:           "",
				Hash:            hash,
				OracleSignature: nil,
			}, currentValidators, ownValidatorIndex, VoteTypeDisAgree, f.privValidator)
			if err != nil {
				f.Logger.Error("FnConsensusError: unable to add disagree vote to current voteset, ignoring...", "error", err)
				return
			}
		}

		didWeContribute = true
		hasOurVoteSetChanged = true
	}

	// If our vote havent't changed, no need to annonce it, as
	// we would have already annonunced it last time it changed
	// This could mean no new additions happened on our existing voteset, and
	// by logic other flags also will be false
	if !hasOurVoteSetChanged {
		return
	}

	marshalledBytes, err := currentVoteSet.Marshal()
	if err != nil {
		f.Logger.Error(fmt.Sprintf("FnConsensusReactor: Unable to marshal currentVoteSet at FnID: %s", fnID))
		return
	}

	f.peerMapMtx.RLock()
	for peerID, peer := range f.connectedPeers {

		// If we didnt contribute to remote vote, no need to pass it to sender
		// If this is false, then we must not have achieved Maj23
		if !didWeContribute {
			if peerID == sender.ID() {
				continue
			}
		}

		// TODO: Handle timeout
		peer.Send(FnVoteSetChannel, marshalledBytes)
	}
	f.peerMapMtx.RUnlock()
}

// Receive is called when msgBytes is received from peer.
//
// NOTE reactor can not keep msgBytes around after Receive completes without
// copying.
//
// CONTRACT: msgBytes are not nil.
func (f *FnConsensusReactor) Receive(chID byte, sender p2p.Peer, msgBytes []byte) {

	switch chID {
	case FnVoteSetChannel:
		f.handleVoteSetChannelMessage(sender, msgBytes)
		break
	case FnMaj23Channel:
		f.handleMaj23VoteSetChannel(sender, msgBytes)
		break
	default:
		f.Logger.Error("FnConsensusReactor: Unknown channel: %v", chID)
	}
}
