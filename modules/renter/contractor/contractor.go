package contractor

// TODO: We are in the middle of migrating the contractor to a new concurrency
// model. The contractor should never call out to another package while under a
// lock (except for the proto package). This is because the renter is going to
// start calling contractor methods while holding the renter lock, so we need to
// be absolutely confident that no contractor thread will attempt to grab a
// renter lock.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/modules/renter/proto"
	"github.com/NebulousLabs/Sia/persist"
	siasync "github.com/NebulousLabs/Sia/sync"
	"github.com/NebulousLabs/Sia/types"
)

var (
	errNilCS     = errors.New("cannot create contractor with nil consensus set")
	errNilTpool  = errors.New("cannot create contractor with nil transaction pool")
	errNilWallet = errors.New("cannot create contractor with nil wallet")

	// COMPATv1.0.4-lts
	// metricsContractID identifies a special contract that contains aggregate
	// financial metrics from older contractors
	metricsContractID = types.FileContractID{'m', 'e', 't', 'r', 'i', 'c', 's'}
)

// A Contractor negotiates, revises, renews, and provides access to file
// contracts.
type Contractor struct {
	// dependencies
	cs      consensusSet
	hdb     hostDB
	log     *persist.Logger
	persist persister
	mu      sync.RWMutex
	tg      siasync.ThreadGroup
	tpool   transactionPool
	wallet  wallet

	// Only one thread should be performing contract maintenance at a time.
	interruptMaintenance chan struct{}
	maintenanceLock      siasync.TryMutex

	allowance     modules.Allowance
	blockHeight   types.BlockHeight
	currentPeriod types.BlockHeight
	lastChange    modules.ConsensusChangeID

	downloaders map[types.FileContractID]*hostDownloader
	editors     map[types.FileContractID]*hostEditor
	renewing    map[types.FileContractID]bool // prevent revising during renewal
	revising    map[types.FileContractID]bool // prevent overlapping revisions

	// The contract utility values are not persisted in any way, instead get
	// set based on the values in the hostdb at startup. During startup, the
	// 'managedMarkContractsUtility' needs to be called so that the utility is
	// set correctly.
	contracts         *proto.ContractSet
	contractUtilities map[types.FileContractID]modules.ContractUtility
	oldContracts      map[types.FileContractID]modules.RenterContract
	renewedIDs        map[types.FileContractID]types.FileContractID
}

// resolveID returns the ID of the most recent renewal of id.
func (c *Contractor) resolveID(id types.FileContractID) types.FileContractID {
	newID, exists := c.renewedIDs[id]
	for exists {
		id = newID
		newID, exists = c.renewedIDs[id]
	}
	return id
}

// Allowance returns the current allowance.
func (c *Contractor) Allowance() modules.Allowance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.allowance
}

// PeriodSpending returns the amount spent on contracts during the current
// billing period.
func (c *Contractor) PeriodSpending() modules.ContractorSpending {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var spending modules.ContractorSpending
	for _, contract := range c.contracts.ViewAll() {
		spending.ContractSpending = spending.ContractSpending.Add(contract.TotalCost)
		spending.DownloadSpending = spending.DownloadSpending.Add(contract.DownloadSpending)
		spending.UploadSpending = spending.UploadSpending.Add(contract.UploadSpending)
		spending.StorageSpending = spending.StorageSpending.Add(contract.StorageSpending)
		// TODO: fix PreviousContracts
		// for _, pre := range contract.PreviousContracts {
		// 	spending.ContractSpending = spending.ContractSpending.Add(pre.TotalCost)
		// 	spending.DownloadSpending = spending.DownloadSpending.Add(pre.DownloadSpending)
		// 	spending.UploadSpending = spending.UploadSpending.Add(pre.UploadSpending)
		// 	spending.StorageSpending = spending.StorageSpending.Add(pre.StorageSpending)
		// }
	}
	allSpending := spending.ContractSpending.Add(spending.DownloadSpending).Add(spending.UploadSpending).Add(spending.StorageSpending)

	// If there is no allowance, the unspent funds are 0
	if !c.allowance.Funds.IsZero() {
		spending.Unspent = c.allowance.Funds.Sub(allSpending)
	}
	return spending
}

// ContractByID returns the contract with the id specified, if it exists. The
// contract will be resolved if possible to the most recent child contract.
func (c *Contractor) ContractByID(id types.FileContractID) (modules.RenterContract, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.contracts.View(c.resolveID(id))
}

// Contracts returns the contracts formed by the contractor in the current
// allowance period. Only contracts formed with currently online hosts are
// returned.
func (c *Contractor) Contracts() []modules.RenterContract {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.contracts.ViewAll()
}

// ContractUtility returns the utility fields for the given contract.
func (c *Contractor) ContractUtility(id types.FileContractID) (modules.ContractUtility, bool) {
	c.mu.RLock()
	utility, exists := c.contractUtilities[c.resolveID(id)]
	c.mu.RUnlock()
	return utility, exists
}

// CurrentPeriod returns the height at which the current allowance period
// began.
func (c *Contractor) CurrentPeriod() types.BlockHeight {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentPeriod
}

// ResolveID returns the ID of the most recent renewal of id.
func (c *Contractor) ResolveID(id types.FileContractID) types.FileContractID {
	c.mu.RLock()
	newID := c.resolveID(id)
	c.mu.RUnlock()
	return newID
}

// Close closes the Contractor.
func (c *Contractor) Close() error {
	return c.tg.Stop()
}

// New returns a new Contractor.
func New(cs consensusSet, wallet walletShim, tpool transactionPool, hdb hostDB, persistDir string) (*Contractor, error) {
	// Check for nil inputs.
	if cs == nil {
		return nil, errNilCS
	}
	if wallet == nil {
		return nil, errNilWallet
	}
	if tpool == nil {
		return nil, errNilTpool
	}

	// Create the persist directory if it does not yet exist.
	if err := os.MkdirAll(persistDir, 0700); err != nil {
		return nil, err
	}

	// Convert the old persist file(s), if necessary. This must occur before
	// loading the contract set.
	if err := convertPersist(persistDir); err != nil {
		return nil, err
	}

	// Create the contract set.
	contractSet, err := proto.NewContractSet(filepath.Join(persistDir, "contracts"))
	if err != nil {
		return nil, err
	}
	// Create the logger.
	logger, err := persist.NewFileLogger(filepath.Join(persistDir, "contractor.log"))
	if err != nil {
		return nil, err
	}

	// Create Contractor using production dependencies.
	return newContractor(cs, &walletBridge{w: wallet}, tpool, hdb, contractSet, newPersist(persistDir), logger)
}

// newContractor creates a Contractor using the provided dependencies.
func newContractor(cs consensusSet, w wallet, tp transactionPool, hdb hostDB, contractSet *proto.ContractSet, p persister, l *persist.Logger) (*Contractor, error) {
	// Create the Contractor object.
	c := &Contractor{
		cs:      cs,
		hdb:     hdb,
		log:     l,
		persist: p,
		tpool:   tp,
		wallet:  w,

		interruptMaintenance: make(chan struct{}),

		contracts:         contractSet,
		downloaders:       make(map[types.FileContractID]*hostDownloader),
		editors:           make(map[types.FileContractID]*hostEditor),
		contractUtilities: make(map[types.FileContractID]modules.ContractUtility),
		oldContracts:      make(map[types.FileContractID]modules.RenterContract),
		renewedIDs:        make(map[types.FileContractID]types.FileContractID),
		renewing:          make(map[types.FileContractID]bool),
		revising:          make(map[types.FileContractID]bool),
	}
	// Close the contract set and logger upon shutdown.
	c.tg.AfterStop(func() {
		if err := c.contracts.Close(); err != nil {
			c.log.Println("Failed to close contract set:", err)
		}
		if err := c.log.Close(); err != nil {
			fmt.Println("Failed to close the contractor logger:", err)
		}
	})

	// Load the prior persistence structures.
	err := c.load()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Mark contract utility.
	c.managedMarkContractsUtility()

	// Subscribe to the consensus set.
	err = cs.ConsensusSetSubscribe(c, c.lastChange, c.tg.StopChan())
	if err == modules.ErrInvalidConsensusChangeID {
		// Reset the contractor consensus variables and try rescanning.
		c.blockHeight = 0
		c.lastChange = modules.ConsensusChangeBeginning
		err = cs.ConsensusSetSubscribe(c, c.lastChange, c.tg.StopChan())
	}
	if err != nil {
		return nil, errors.New("contractor subscription failed: " + err.Error())
	}
	// Unsubscribe from the consensus set upon shutdown.
	c.tg.OnStop(func() {
		cs.Unsubscribe(c)
	})

	// We may have upgraded persist or resubscribed. Save now so that we don't
	// lose our work.
	c.mu.Lock()
	err = c.save()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	return c, nil
}
