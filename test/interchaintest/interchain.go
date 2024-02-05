package interchaintest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"
	setup "github.com/icon-project/centralized-relay/test"
	"github.com/icon-project/centralized-relay/test/chains"
	"github.com/icon-project/centralized-relay/test/interchaintest/_internal/dockerutil"
	"github.com/icon-project/centralized-relay/test/interchaintest/ibc"
	"github.com/icon-project/centralized-relay/test/interchaintest/testreporter"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Interchain represents a full IBC network, encompassing a collection of
// one or more chains, one or more relayer instances, and initial account configuration.
type Interchain struct {
	log *zap.Logger

	// Map of chain reference to chain ID.
	chains map[chains.Chain]string

	// Map of relayer reference to user-supplied instance name.
	relayers map[ibc.Relayer]string

	// Key: relayer and path name; Value: the two chains being linked.
	links map[relayerPath]interchainLink

	// Set to true after Build is called once.
	built bool

	// Map of relayer-chain pairs to address and mnemonic, set during Build().
	// Not yet exposed through any exported API.
	relayerWallets map[relayerChain]ibc.Wallet

	// Map of chain to additional genesis wallets to include at chain start.
	AdditionalGenesisWallets map[chains.Chain][]ibc.WalletAmount

	// Set during Build and cleaned up in the Close method.
	cs *chainSet
}

type interchainLink struct {
	chains []chains.Chain
}

// NewInterchain returns a new Interchain.
//
// Typical usage involves multiple calls to AddChain, one or more calls to AddRelayer,
// one or more calls to AddLink, and then finally a single call to Build.
func NewInterchain() *Interchain {
	return &Interchain{
		log: zap.NewNop(),

		chains:   make(map[chains.Chain]string),
		relayers: make(map[ibc.Relayer]string),

		links: make(map[relayerPath]interchainLink),
	}
}

// relayerPath is a tuple of a relayer and a path name.
type relayerPath struct {
	Relayer ibc.Relayer
	Path    string
}

// AddChain adds the given chain to the Interchain,
// using the chain ID reported by the chain's config.
// If the given chain already exists,
// or if another chain with the same configured chain ID exists, AddChain panics.
func (ic *Interchain) AddChain(chain chains.Chain, additionalGenesisWallets ...ibc.WalletAmount) *Interchain {
	if chain == nil {
		panic(fmt.Errorf("cannot add nil chain"))
	}

	newID := chain.Config().ChainID
	newName := chain.Config().Name

	for c, id := range ic.chains {
		if c == chain {
			panic(fmt.Errorf("chain %v was already added", c))
		}
		if id == newID {
			panic(fmt.Errorf("a chain with ID %s already exists", id))
		}
		if c.Config().Name == newName {
			panic(fmt.Errorf("a chain with name %s already exists", newName))
		}
	}

	ic.chains[chain] = newID

	if len(additionalGenesisWallets) == 0 {
		return ic
	}

	if ic.AdditionalGenesisWallets == nil {
		ic.AdditionalGenesisWallets = make(map[chains.Chain][]ibc.WalletAmount)
	}
	ic.AdditionalGenesisWallets[chain] = additionalGenesisWallets

	return ic
}

// AddRelayer adds the given relayer with the given name to the Interchain.
func (ic *Interchain) AddRelayer(relayer ibc.Relayer, name string) *Interchain {
	if relayer == nil {
		panic(fmt.Errorf("cannot add nil relayer"))
	}

	for r, n := range ic.relayers {
		if r == relayer {
			panic(fmt.Errorf("relayer %v was already added", r))
		}
		if n == name {
			panic(fmt.Errorf("a relayer with name %s already exists", n))
		}
	}

	ic.relayers[relayer] = name
	return ic
}

// InterchainLink describes a link between chains,
// by specifying the chain names, the relayer name,
// and the name of the path to create.
type InterchainLink struct {
	// Chains involved.
	Chains map[string]chains.Chain

	// Relayer to use for link.
	Relayer ibc.Relayer
}

// AddLink adds the given link to the Interchain.
// If any validation fails, AddLink panics.
func (ic *Interchain) AddLink(link InterchainLink) *Interchain {
	for name, chain := range link.Chains {
		if ic.chains[chain] != name {
			panic(fmt.Errorf("chain %v was never added to Interchain", chain))
		}
	}

	key := relayerPath{
		Relayer: link.Relayer,
	}

	if _, exists := ic.links[key]; exists {
		panic(fmt.Errorf("relayer %q already has a path named %q", key.Relayer, key.Path))
	}

	ic.links[key] = interchainLink{
		chains: make([]chains.Chain, 0, len(link.Chains)),
	}
	return ic
}

// InterchainBuildOptions describes configuration for (*Interchain).Build.
type InterchainBuildOptions struct {
	TestName string

	Client    *client.Client
	NetworkID string

	// If set, ic.Build does not create paths or links in the relayer,
	// but it does still configure keys and wallets for declared relayer-chain links.
	// This is useful for tests that need lower-level access to configuring relayers.
	SkipPathCreation bool

	// Optional. Git sha for test invocation. Once Go 1.18 supported,
	// may be deprecated in favor of runtime/debug.ReadBuildInfo.
	GitSha string

	// If set, saves block history to a sqlite3 database to aid debugging.
	BlockDatabaseFile string
}

func (ic *Interchain) BuildChains(ctx context.Context, rep *testreporter.RelayerExecReporter, opts InterchainBuildOptions) error {
	if ic.built {
		panic(fmt.Errorf("Interchain.Build called more than once"))
	}
	ic.built = true

	chains := make([]chains.Chain, 0, len(ic.chains))
	for chain := range ic.chains {
		chains = append(chains, chain)
	}
	ic.cs = newChainSet(ic.log, chains)

	// Initialize the chains (pull docker images, etc.).
	if err := ic.cs.Initialize(ctx, opts.TestName, opts.Client, opts.NetworkID); err != nil {
		return fmt.Errorf("failed to initialize chains: %w", err)
	}

	err := ic.generateRelayerWallets(ctx) // Build the relayer wallet mapping.
	if err != nil {
		return err
	}

	walletAmounts, err := ic.genesisWalletAmounts(ctx)
	if err != nil {
		// Error already wrapped with appropriate detail.
		return err
	}

	if err := ic.cs.Start(ctx, opts.TestName, walletAmounts); err != nil {
		return fmt.Errorf("failed to start chains: %w", err)
	}

	if err := ic.cs.TrackBlocks(ctx, opts.TestName, opts.BlockDatabaseFile, opts.GitSha); err != nil {
		return fmt.Errorf("failed to track blocks: %w", err)
	}

	return nil
}

func (ic *Interchain) BuildRelayer(ctx context.Context, rep *testreporter.RelayerExecReporter, opts InterchainBuildOptions) error {
	// Possible optimization: each relayer could be configured concurrently.
	// But we are only testing with a single relayer so far, so we don't need this yet.
	config := ibc.RelayerConfig{
		Global: struct {
			ApiListenAddr  int    `yaml:"api-listen-addr"`
			Timeout        string `yaml:"timeout"`
			Memo           string `yaml:"memo"`
			LightCacheSize int    `yaml:"light-cache-size"`
		}{
			ApiListenAddr:  5183,
			Timeout:        "10s",
			Memo:           "",
			LightCacheSize: 20,
		},
		Chains: make(map[string]interface{}),
	}
	for r, nodes := range ic.relayerChains() {
		for _, c := range nodes {
			chainName := ic.chains[c]
			wallet := ic.relayerWallets[relayerChain{R: r, C: c}]
			content, _ := c.GetRelayConfig(ctx, r.HomeDir()+"/.centralized-relay", wallet.KeyName())
			chainConfig := make(map[string]interface{})
			_ = yaml.Unmarshal(content, &chainConfig)
			config.Chains[chainName] = chainConfig
			key, err := r.GetKeystore(c.Config().Type, wallet)
			if err != nil {
				return fmt.Errorf("failed to get keystore %s for chain %s: %w", ic.relayers[r], chainName, err)
			}

			if err := r.RestoreKeystore(ctx, key, c.Config().ChainID, wallet.KeyName()); err != nil {
				return fmt.Errorf("failed to restore key to relayer %s for chain %s: %w", ic.relayers[r], chainName, err)
			}

		}
	}
	content, _ := yaml.Marshal(config)
	for r := range ic.relayerChains() {
		if err := r.CreateConfig(ctx, content); err != nil {
			return fmt.Errorf("failed to restore config to relayer %s : %w", ic.relayers[r], err)
		}
	}
	return nil
}

// WithLog sets the logger on the interchain object.
// Usually the default nop logger is fine, but sometimes it can be helpful
// to see more verbose logs, typically by passing zaptest.NewLogger(t).
func (ic *Interchain) WithLog(log *zap.Logger) *Interchain {
	ic.log = log
	return ic
}

// Close cleans up any resources created during Build,
// and returns any relevant errors.
func (ic *Interchain) Close() error {
	return ic.cs.Close()
}

func (ic *Interchain) genesisWalletAmounts(ctx context.Context) (map[chains.Chain][]ibc.WalletAmount, error) {
	// Faucet addresses are created separately because they need to be explicitly added to the chains.
	faucetWallet, err := ic.cs.CreateCommonAccount(ctx, setup.FaucetAccountKeyName)
	if err != nil {
		return nil, fmt.Errorf("failed to create faucet accounts: %w", err)
	}

	// Wallet amounts for genesis.
	walletAmounts := make(map[chains.Chain][]ibc.WalletAmount, len(ic.cs.chains))

	// Add faucet for each chain first.
	for c := range ic.chains {
		// The values are nil at this point, so it is safe to directly assign the slice.
		walletAmounts[c] = []ibc.WalletAmount{
			{
				Address: faucetWallet[c].FormattedAddress(),
				Amount:  100_000_000_000_000, // Faucet wallet gets 100T units of denom.
			},
		}

		if ic.AdditionalGenesisWallets != nil {
			walletAmounts[c] = append(walletAmounts[c], ic.AdditionalGenesisWallets[c]...)
		}
	}

	// Then add all defined relayer wallets.
	for rc, wallet := range ic.relayerWallets {
		c := rc.C
		walletAmounts[c] = append(walletAmounts[c], ibc.WalletAmount{
			Address: wallet.FormattedAddress(),
			Amount:  100_000_000_000_000, // Every wallet gets 1t units of denom.
		})
	}

	return walletAmounts, nil
}

// generateRelayerWallets populates ic.relayerWallets.
func (ic *Interchain) generateRelayerWallets(ctx context.Context) error {
	if ic.relayerWallets != nil {
		panic(fmt.Errorf("cannot call generateRelayerWallets more than once"))
	}

	relayerChains := ic.relayerChains()
	ic.relayerWallets = make(map[relayerChain]ibc.Wallet, len(relayerChains))
	for r, chains := range relayerChains {
		for _, c := range chains {
			// Just an ephemeral unique name, only for the local use of the keyring.
			accountName := "relayer-" + c.Config().Name
			newWallet, err := c.BuildRelayerWallet(ctx, accountName)
			if err != nil {
				return err
			}
			ic.relayerWallets[relayerChain{R: r, C: c}] = newWallet
		}
	}

	return nil
}

// relayerChain is a tuple of a Relayer and a Chain.
type relayerChain struct {
	R ibc.Relayer
	C chains.Chain
}

// relayerChains builds a mapping of relayers to the chains they connect to.
// The order of the chains is arbitrary.
func (ic *Interchain) relayerChains() map[ibc.Relayer][]chains.Chain {
	// First, collect a mapping of relayers to sets of chains,
	// so we don't have to manually deduplicate entries.
	uniq := make(map[ibc.Relayer]map[chains.Chain]struct{}, len(ic.relayers))

	for rp, link := range ic.links {
		r := rp.Relayer
		if uniq[r] == nil {
			uniq[r] = make(map[chains.Chain]struct{}, 2) // Adding at least 2 chains per relayer.
		}
		uniq[r][link.chains[0]] = struct{}{}
		uniq[r][link.chains[1]] = struct{}{}
	}

	// Then convert the sets to slices.
	out := make(map[ibc.Relayer][]chains.Chain, len(uniq))
	for r, chainSet := range uniq {
		chains := make([]chains.Chain, 0, len(chainSet))
		for chain := range chainSet {
			chains = append(chains, chain)
		}

		out[r] = chains
	}
	return out
}

func CreateLogFile(name string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}
	fpath := filepath.Join(home, ".interchaintest", "logs")
	err = os.MkdirAll(fpath, 0o755)
	if err != nil {
		return nil, fmt.Errorf("mkdirall: %w", err)
	}
	return os.Create(filepath.Join(fpath, name))
}

// DefaultBlockDatabaseFilepath is the default filepath to the sqlite database for tracking blocks and transactions.
func DefaultBlockDatabaseFilepath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(home, ".interchaintest", "databases", "block.db")
}

// KeepDockerVolumesOnFailure sets whether volumes associated with a particular test
// are retained or deleted following a test failure.
//
// The value is false by default, but can be initialized to true by setting the
// environment variable IBCTEST_SKIP_FAILURE_CLEANUP to a non-empty value.
// Alternatively, importers of the interchaintest package may call KeepDockerVolumesOnFailure(true).
func KeepDockerVolumesOnFailure(b bool) {
	dockerutil.KeepVolumesOnFailure = b
}

// DockerSetup returns a new Docker Client and the ID of a configured network, associated with t.
//
// If any part of the setup fails, t.Fatal is called.
func DockerSetup(t *testing.T) (*client.Client, string) {
	t.Helper()
	origKeep := dockerutil.KeepVolumesOnFailure
	defer func() {
		dockerutil.KeepVolumesOnFailure = origKeep
	}()
	return dockerutil.DockerSetup(t)
}
