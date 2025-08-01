// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/svchost"
	"github.com/posener/complete"
	"github.com/zclconf/go-cty/cty"
	otelAttr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	backendInit "github.com/opentofu/opentofu/internal/backend/init"
	"github.com/opentofu/opentofu/internal/cloud"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/getproviders"
	"github.com/opentofu/opentofu/internal/providercache"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
	"github.com/opentofu/opentofu/internal/tofumigrate"
	"github.com/opentofu/opentofu/internal/tracing"
	tfversion "github.com/opentofu/opentofu/version"
)

// InitCommand is a Command implementation that takes a Terraform
// module and clones it to the working directory.
type InitCommand struct {
	Meta
}

func (c *InitCommand) Run(args []string) int {
	ctx := c.CommandContext()

	ctx, span := tracing.Tracer().Start(ctx, "Init")
	defer span.End()

	var flagFromModule, flagLockfile, testsDirectory string
	var flagBackend, flagCloud, flagGet, flagUpgrade bool
	var flagPluginPath FlagStringSlice
	flagConfigExtra := newRawFlags("-backend-config")

	args = c.Meta.process(args)
	cmdFlags := c.Meta.extendedFlagSet("init")
	cmdFlags.BoolVar(&flagBackend, "backend", true, "")
	cmdFlags.BoolVar(&flagCloud, "cloud", true, "")
	cmdFlags.Var(flagConfigExtra, "backend-config", "")
	cmdFlags.StringVar(&flagFromModule, "from-module", "", "copy the source of the given module into the directory before init")
	cmdFlags.BoolVar(&flagGet, "get", true, "")
	cmdFlags.BoolVar(&c.forceInitCopy, "force-copy", false, "suppress prompts about copying state data")
	cmdFlags.BoolVar(&c.Meta.stateLock, "lock", true, "lock state")
	cmdFlags.DurationVar(&c.Meta.stateLockTimeout, "lock-timeout", 0, "lock timeout")
	cmdFlags.BoolVar(&c.reconfigure, "reconfigure", false, "reconfigure")
	cmdFlags.BoolVar(&c.migrateState, "migrate-state", false, "migrate state")
	cmdFlags.BoolVar(&flagUpgrade, "upgrade", false, "")
	cmdFlags.Var(&flagPluginPath, "plugin-dir", "plugin directory")
	cmdFlags.StringVar(&flagLockfile, "lockfile", "", "Set a dependency lockfile mode")
	cmdFlags.BoolVar(&c.Meta.ignoreRemoteVersion, "ignore-remote-version", false, "continue even if remote and local OpenTofu versions are incompatible")
	cmdFlags.StringVar(&testsDirectory, "test-directory", "tests", "test-directory")
	cmdFlags.BoolVar(&c.outputInJSON, "json", false, "json")
	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	if c.outputInJSON {
		c.Meta.color = false
		c.Meta.Color = false
		c.oldUi = c.Ui
		c.Ui = &WrappedUi{
			cliUi:        c.oldUi,
			jsonView:     views.NewJSONView(c.View),
			outputInJSON: true,
		}
	}

	backendFlagSet := arguments.FlagIsSet(cmdFlags, "backend")
	cloudFlagSet := arguments.FlagIsSet(cmdFlags, "cloud")

	switch {
	case backendFlagSet && cloudFlagSet:
		c.Ui.Error("The -backend and -cloud options are aliases of one another and mutually-exclusive in their use")
		return 1
	case backendFlagSet:
		flagCloud = flagBackend
	case cloudFlagSet:
		flagBackend = flagCloud
	}

	if c.migrateState && c.reconfigure {
		c.Ui.Error("The -migrate-state and -reconfigure options are mutually-exclusive")
		return 1
	}

	// Copying the state only happens during backend migration, so setting
	// -force-copy implies -migrate-state
	if c.forceInitCopy {
		c.migrateState = true
	}

	var diags tfdiags.Diagnostics

	if len(flagPluginPath) > 0 {
		c.pluginPath = flagPluginPath
	}

	// Validate the arg count and get the working directory
	args = cmdFlags.Args()
	path, err := modulePath(args)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	if err := c.storePluginPath(c.pluginPath); err != nil {
		c.Ui.Error(fmt.Sprintf("Error saving -plugin-path values: %s", err))
		return 1
	}

	// Initialization can be aborted by interruption signals
	ctx, done := c.InterruptibleContext(ctx)
	defer done()

	// This will track whether we outputted anything so that we know whether
	// to output a newline before the success message
	var header bool

	if flagFromModule != "" {
		src := flagFromModule

		empty, err := configs.IsEmptyDir(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error validating destination directory: %s", err))
			return 1
		}
		if !empty {
			c.Ui.Error(strings.TrimSpace(errInitCopyNotEmpty))
			return 1
		}

		c.Ui.Output(c.Colorize().Color(fmt.Sprintf(
			"[reset][bold]Copying configuration[reset] from %q...", src,
		)))
		header = true

		hooks := uiModuleInstallHooks{
			Ui:             c.Ui,
			ShowLocalPaths: false, // since they are in a weird location for init
		}

		ctx, span := tracing.Tracer().Start(ctx, "From module", trace.WithAttributes(
			otelAttr.String("opentofu.module_source", src),
		))
		defer span.End()

		initDirFromModuleAbort, initDirFromModuleDiags := c.initDirFromModule(ctx, path, src, hooks)
		diags = diags.Append(initDirFromModuleDiags)
		if initDirFromModuleAbort || initDirFromModuleDiags.HasErrors() {
			c.showDiagnostics(diags)
			tracing.SetSpanError(span, initDirFromModuleDiags)
			span.End()
			return 1
		}

		c.Ui.Output("")
	}

	// If our directory is empty, then we're done. We can't get or set up
	// the backend with an empty directory.
	empty, err := configs.IsEmptyDir(path)
	if err != nil {
		diags = diags.Append(fmt.Errorf("Error checking configuration: %w", err))
		c.showDiagnostics(diags)
		return 1
	}
	if empty {
		c.Ui.Output(c.Colorize().Color(strings.TrimSpace(outputInitEmpty)))
		return 0
	}

	// Load just the root module to begin backend and module initialization
	rootModEarly, earlyConfDiags := c.loadSingleModuleWithTests(ctx, path, testsDirectory)

	// There may be parsing errors in config loading but these will be shown later _after_
	// checking for core version requirement errors. Not meeting the version requirement should
	// be the first error displayed if that is an issue, but other operations are required
	// before being able to check core version requirements.
	if rootModEarly == nil {
		c.Ui.Error(c.Colorize().Color(strings.TrimSpace(errInitConfigError)))
		diags = diags.Append(earlyConfDiags)
		c.showDiagnostics(diags)

		return 1
	}

	var enc encryption.Encryption
	// If backend flag is explicitly set to false i.e -backend=false, we disable state and plan encryption
	if backendFlagSet && !flagBackend {
		enc = encryption.Disabled()
	} else {
		// Load the encryption configuration
		var encDiags tfdiags.Diagnostics
		enc, encDiags = c.EncryptionFromModule(ctx, rootModEarly)
		diags = diags.Append(encDiags)
		if encDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
	}

	var back backend.Backend

	// There may be config errors or backend init errors but these will be shown later _after_
	// checking for core version requirement errors.
	var backDiags tfdiags.Diagnostics
	var backendOutput bool

	switch {
	case flagCloud && rootModEarly.CloudConfig != nil:
		back, backendOutput, backDiags = c.initCloud(ctx, rootModEarly, flagConfigExtra, enc)
	case flagBackend:
		back, backendOutput, backDiags = c.initBackend(ctx, rootModEarly, flagConfigExtra, enc)
	default:
		// load the previously-stored backend config
		back, backDiags = c.Meta.backendFromState(ctx, enc.State())
	}
	if backendOutput {
		header = true
	}

	var state *states.State

	// If we have a functional backend (either just initialized or initialized
	// on a previous run) we'll use the current state as a potential source
	// of provider dependencies.
	if back != nil {
		c.ignoreRemoteVersionConflict(back)
		workspace, err := c.Workspace(ctx)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error selecting workspace: %s", err))
			return 1
		}
		sMgr, err := back.StateMgr(ctx, workspace)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error loading state: %s", err))
			return 1
		}

		if err := sMgr.RefreshState(context.TODO()); err != nil {
			c.Ui.Error(fmt.Sprintf("Error refreshing state: %s", err))
			return 1
		}

		state = sMgr.State()
	}

	if flagGet {
		modsOutput, modsAbort, modsDiags := c.getModules(ctx, path, testsDirectory, rootModEarly, flagUpgrade)
		diags = diags.Append(modsDiags)
		if modsAbort || modsDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
		if modsOutput {
			header = true
		}
	}

	// With all of the modules (hopefully) installed, we can now try to load the
	// whole configuration tree.
	config, confDiags := c.loadConfigWithTests(ctx, path, testsDirectory)
	// configDiags will be handled after the version constraint check, since an
	// incorrect version of tofu may be producing errors for configuration
	// constructs added in later versions.

	// Before we go further, we'll check to make sure none of the modules in
	// the configuration declare that they don't support this OpenTofu
	// version, so we can produce a version-related error message rather than
	// potentially-confusing downstream errors.
	versionDiags := tofu.CheckCoreVersionRequirements(config)
	if versionDiags.HasErrors() {
		c.showDiagnostics(versionDiags)
		return 1
	}

	// We've passed the core version check, now we can show errors from the
	// configuration and backend initialization.

	// Now, we can check the diagnostics from the early configuration and the
	// backend.
	diags = diags.Append(earlyConfDiags.StrictDeduplicateMerge(backDiags))
	if earlyConfDiags.HasErrors() {
		c.Ui.Error(strings.TrimSpace(errInitConfigError))
		c.showDiagnostics(diags)
		return 1
	}

	// Now, we can show any errors from initializing the backend, but we won't
	// show the errInitConfigError preamble as we didn't detect problems with
	// the early configuration.
	if backDiags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	// If everything is ok with the core version check and backend initialization,
	// show other errors from loading the full configuration tree.
	diags = diags.Append(confDiags)
	if confDiags.HasErrors() {
		c.Ui.Error(strings.TrimSpace(errInitConfigError))
		c.showDiagnostics(diags)
		return 1
	}

	if cb, ok := back.(*cloud.Cloud); ok {
		if c.RunningInAutomation {
			if err := cb.AssertImportCompatible(config); err != nil {
				diags = diags.Append(tfdiags.Sourceless(tfdiags.Error, "Compatibility error", err.Error()))
				c.showDiagnostics(diags)
				return 1
			}
		}
	}

	if state != nil {
		// Since we now have the full configuration loaded, we can use it to migrate the in-memory state view
		// prior to fetching providers.
		migratedState, migrateDiags := tofumigrate.MigrateStateProviderAddresses(config, state)
		diags = diags.Append(migrateDiags)
		if migrateDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
		state = migratedState
	}

	// Now that we have loaded all modules, check the module tree for missing providers.
	providersOutput, providersAbort, providerDiags := c.getProviders(ctx, config, state, flagUpgrade, flagPluginPath, flagLockfile)
	diags = diags.Append(providerDiags)
	if providersAbort || providerDiags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}
	if providersOutput {
		header = true
	}

	// If we outputted information, then we need to output a newline
	// so that our success message is nicely spaced out from prior text.
	if header {
		c.Ui.Output("")
	}

	// If we accumulated any warnings along the way that weren't accompanied
	// by errors then we'll output them here so that the success message is
	// still the final thing shown.
	c.showDiagnostics(diags)
	_, cloud := back.(*cloud.Cloud)
	output := outputInitSuccess
	if cloud {
		output = outputInitSuccessCloud
	}

	c.Ui.Output(c.Colorize().Color(strings.TrimSpace(output)))

	if !c.RunningInAutomation {
		// If we're not running in an automation wrapper, give the user
		// some more detailed next steps that are appropriate for interactive
		// shell usage.
		output = outputInitSuccessCLI
		if cloud {
			output = outputInitSuccessCLICloud
		}
		c.Ui.Output(c.Colorize().Color(strings.TrimSpace(output)))
	}
	return 0
}

func (c *InitCommand) getModules(ctx context.Context, path, testsDir string, earlyRoot *configs.Module, upgrade bool) (output bool, abort bool, diags tfdiags.Diagnostics) {
	testModules := false // We can also have modules buried in test files.
	for _, file := range earlyRoot.Tests {
		for _, run := range file.Runs {
			if run.Module != nil {
				testModules = true
			}
		}
	}

	if len(earlyRoot.ModuleCalls) == 0 && !testModules {
		// Nothing to do
		return false, false, nil
	}

	ctx, span := tracing.Tracer().Start(ctx, "Get Modules", trace.WithAttributes(
		otelAttr.Bool("opentofu.modules.upgrade", upgrade),
	))
	defer span.End()

	if upgrade {
		c.Ui.Output(c.Colorize().Color("[reset][bold]Upgrading modules..."))
	} else {
		c.Ui.Output(c.Colorize().Color("[reset][bold]Initializing modules..."))
	}

	hooks := uiModuleInstallHooks{
		Ui:             c.Ui,
		ShowLocalPaths: true,
	}

	installAbort, installDiags := c.installModules(ctx, path, testsDir, upgrade, false, hooks)
	diags = diags.Append(installDiags)

	// At this point, installModules may have generated error diags or been
	// aborted by SIGINT. In any case we continue and the manifest as best
	// we can.

	// Since module installer has modified the module manifest on disk, we need
	// to refresh the cache of it in the loader.
	if c.configLoader != nil {
		if err := c.configLoader.RefreshModules(); err != nil {
			// Should never happen
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to read module manifest",
				fmt.Sprintf("After installing modules, OpenTofu could not re-read the manifest of installed modules. This is a bug in OpenTofu. %s.", err),
			))
		}
	}

	return true, installAbort, diags
}

func (c *InitCommand) initCloud(ctx context.Context, root *configs.Module, extraConfig rawFlags, enc encryption.Encryption) (be backend.Backend, output bool, diags tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "Cloud backend init")
	_ = ctx // prevent staticcheck from complaining to avoid a maintenance hazard of having the wrong ctx in scope here
	defer span.End()

	c.Ui.Output(c.Colorize().Color("\n[reset][bold]Initializing cloud backend..."))

	if len(extraConfig.AllItems()) != 0 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid command-line option",
			"The -backend-config=... command line option is only for state backends, and is not applicable to cloud backend-based configurations.\n\nTo change the set of workspaces associated with this configuration, edit the Cloud configuration block in the root module.",
		))
		return nil, true, diags
	}

	backendConfig := root.CloudConfig.ToBackendConfig()

	opts := &BackendOpts{
		Config: &backendConfig,
		Init:   true,
	}

	back, backDiags := c.Backend(ctx, opts, enc.State())
	diags = diags.Append(backDiags)
	return back, true, diags
}

func (c *InitCommand) initBackend(ctx context.Context, root *configs.Module, extraConfig rawFlags, enc encryption.Encryption) (be backend.Backend, output bool, diags tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "Backend init")
	_ = ctx // prevent staticcheck from complaining to avoid a maintenance hazard of having the wrong ctx in scope here
	defer span.End()

	c.Ui.Output(c.Colorize().Color("\n[reset][bold]Initializing the backend..."))

	var backendConfig *configs.Backend
	var backendConfigOverride hcl.Body
	if root.Backend != nil {
		backendType := root.Backend.Type
		if backendType == "cloud" {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported backend type",
				Detail:   fmt.Sprintf("There is no explicit backend type named %q. To configure cloud backend, declare a 'cloud' block instead.", backendType),
				Subject:  &root.Backend.TypeRange,
			})
			return nil, true, diags
		}

		bf := backendInit.Backend(backendType)
		if bf == nil {
			detail := fmt.Sprintf("There is no backend type named %q.", backendType)
			if msg, removed := backendInit.RemovedBackends[backendType]; removed {
				detail = msg
			}

			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported backend type",
				Detail:   detail,
				Subject:  &root.Backend.TypeRange,
			})
			return nil, true, diags
		}

		b := bf(nil) // This is only used to get the schema, encryption should panic if attempted
		backendSchema := b.ConfigSchema()
		backendConfig = root.Backend

		var overrideDiags tfdiags.Diagnostics
		backendConfigOverride, overrideDiags = c.backendConfigOverrideBody(extraConfig, backendSchema)
		diags = diags.Append(overrideDiags)
		if overrideDiags.HasErrors() {
			return nil, true, diags
		}
	} else {
		// If the user supplied a -backend-config on the CLI but no backend
		// block was found in the configuration, it's likely - but not
		// necessarily - a mistake. Return a warning.
		if !extraConfig.Empty() {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Missing backend configuration",
				`-backend-config was used without a "backend" block in the configuration.

If you intended to override the default local backend configuration,
no action is required, but you may add an explicit backend block to your
configuration to clear this warning:

terraform {
  backend "local" {}
}

However, if you intended to override a defined backend, please verify that
the backend configuration is present and valid.
`,
			))
		}
	}

	opts := &BackendOpts{
		Config:         backendConfig,
		ConfigOverride: backendConfigOverride,
		Init:           true,
	}

	back, backDiags := c.Backend(ctx, opts, enc.State())
	diags = diags.Append(backDiags)
	return back, true, diags
}

// Load the complete module tree, and fetch any missing providers.
// This method outputs its own Ui.
func (c *InitCommand) getProviders(ctx context.Context, config *configs.Config, state *states.State, upgrade bool, pluginDirs []string, flagLockfile string) (output, abort bool, diags tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "Get Providers")
	defer span.End()

	// Dev overrides cause the result of "tofu init" to be irrelevant for
	// any overridden providers, so we'll warn about it to avoid later
	// confusion when OpenTofu ends up using a different provider than the
	// lock file called for.
	diags = diags.Append(c.providerDevOverrideInitWarnings())

	// First we'll collect all the provider dependencies we can see in the
	// configuration and the state.
	reqs, qualifs, hclDiags := config.ProviderRequirements()
	diags = diags.Append(hclDiags)
	if hclDiags.HasErrors() {
		return false, true, diags
	}
	if state != nil {
		stateReqs := state.ProviderRequirements()
		reqs = reqs.Merge(stateReqs)
	}

	potentialProviderConflicts := make(map[string][]string)

	for providerAddr := range reqs {
		if providerAddr.Namespace == "hashicorp" || providerAddr.Namespace == "opentofu" {
			potentialProviderConflicts[providerAddr.Type] = append(potentialProviderConflicts[providerAddr.Type], providerAddr.ForDisplay())
		}

		if providerAddr.IsLegacy() {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid legacy provider address",
				fmt.Sprintf(
					"This configuration or its associated state refers to the unqualified provider %q.\n\nYou must complete the Terraform 0.13 upgrade process before upgrading to later versions.",
					providerAddr.Type,
				),
			))
		}
	}

	for name, addrs := range potentialProviderConflicts {
		if len(addrs) > 1 {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Potential provider misconfiguration",
				fmt.Sprintf(
					"OpenTofu has detected multiple providers of type %s (%s) which may be a misconfiguration.\n\nIf this is intentional you can ignore this warning",
					name,
					strings.Join(addrs, ", "),
				),
			))
		}
	}

	previousLocks, moreDiags := c.lockedDependenciesWithPredecessorRegistryShimmed()
	diags = diags.Append(moreDiags)

	if diags.HasErrors() {
		return false, true, diags
	}

	var inst *providercache.Installer
	if len(pluginDirs) == 0 {
		// By default we use a source that looks for providers in all of the
		// standard locations, possibly customized by the user in CLI config.
		inst = c.providerInstaller()
	} else {
		// If the user passes at least one -plugin-dir then that circumvents
		// the usual sources and forces OpenTofu to consult only the given
		// directories. Anything not available in one of those directories
		// is not available for installation.
		source := c.providerCustomLocalDirectorySource(ctx, pluginDirs)
		inst = c.providerInstallerCustomSource(source)

		// The default (or configured) search paths are logged earlier, in provider_source.go
		// Log that those are being overridden by the `-plugin-dir` command line options
		log.Println("[DEBUG] init: overriding provider plugin search paths")
		log.Printf("[DEBUG] will search for provider plugins in %s", pluginDirs)
	}

	// We want to print out a nice warning if we don't manage to pull
	// checksums for all our providers. This is tracked via callbacks
	// and incomplete providers are stored here for later analysis.
	var incompleteProviders []string

	// Because we're currently just streaming a series of events sequentially
	// into the terminal, we're showing only a subset of the events to keep
	// things relatively concise. Later it'd be nice to have a progress UI
	// where statuses update in-place, but we can't do that as long as we
	// are shimming our vt100 output to the legacy console API on Windows.
	evts := &providercache.InstallerEvents{
		PendingProviders: func(reqs map[addrs.Provider]getproviders.VersionConstraints) {
			c.Ui.Output(c.Colorize().Color(
				"\n[reset][bold]Initializing provider plugins...",
			))
		},
		ProviderAlreadyInstalled: func(provider addrs.Provider, selectedVersion getproviders.Version, inProviderCache bool) {
			if inProviderCache {
				c.Ui.Info(fmt.Sprintf("- Detected previously-installed %s v%s in the shared cache directory", provider.ForDisplay(), selectedVersion))
			} else {
				c.Ui.Info(fmt.Sprintf("- Using previously-installed %s v%s", provider.ForDisplay(), selectedVersion))
			}
		},
		BuiltInProviderAvailable: func(provider addrs.Provider) {
			c.Ui.Info(fmt.Sprintf("- %s is built in to OpenTofu", provider.ForDisplay()))
		},
		BuiltInProviderFailure: func(provider addrs.Provider, err error) {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid dependency on built-in provider",
				fmt.Sprintf("Cannot use %s: %s.", provider.ForDisplay(), err),
			))
		},
		QueryPackagesBegin: func(provider addrs.Provider, versionConstraints getproviders.VersionConstraints, locked bool) {
			if locked {
				c.Ui.Info(fmt.Sprintf("- Reusing previous version of %s from the dependency lock file", provider.ForDisplay()))
			} else {
				if len(versionConstraints) > 0 {
					c.Ui.Info(fmt.Sprintf("- Finding %s versions matching %q...", provider.ForDisplay(), getproviders.VersionConstraintsString(versionConstraints)))
				} else {
					c.Ui.Info(fmt.Sprintf("- Finding latest version of %s...", provider.ForDisplay()))
				}
			}
		},
		LinkFromCacheBegin: func(provider addrs.Provider, version getproviders.Version, cacheRoot string) {
			c.Ui.Info(fmt.Sprintf("- Using %s v%s from the shared cache directory", provider.ForDisplay(), version))
		},
		FetchPackageBegin: func(provider addrs.Provider, version getproviders.Version, location getproviders.PackageLocation, inProviderCache bool) {
			if inProviderCache {
				c.Ui.Info(fmt.Sprintf("- Installing %s v%s to the shared cache directory...", provider.ForDisplay(), version))
			} else {
				c.Ui.Info(fmt.Sprintf("- Installing %s v%s...", provider.ForDisplay(), version))
			}
		},
		QueryPackagesFailure: func(provider addrs.Provider, err error) {
			switch errorTy := err.(type) {
			case getproviders.ErrProviderNotFound:
				sources := errorTy.Sources
				displaySources := make([]string, len(sources))
				for i, source := range sources {
					displaySources[i] = fmt.Sprintf("  - %s", source)
				}
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to query available provider packages",
					fmt.Sprintf("Could not retrieve the list of available versions for provider %s: %s\n\n%s",
						provider.ForDisplay(), err, strings.Join(displaySources, "\n"),
					),
				))
			case getproviders.ErrRegistryProviderNotKnown:
				// We might be able to suggest an alternative provider to use
				// instead of this one.
				suggestion := fmt.Sprintf("\n\nAll modules should specify their required_providers so that external consumers will get the correct providers when using a module. To see which modules are currently depending on %s, run the following command:\n    tofu providers", provider.ForDisplay())
				alternative := getproviders.MissingProviderSuggestion(ctx, provider, inst.ProviderSource(), reqs)
				if alternative != provider {
					suggestion = fmt.Sprintf(
						"\n\nDid you intend to use %s? If so, you must specify that source address in each module which requires that provider. To see which modules are currently depending on %s, run the following command:\n    tofu providers",
						alternative.ForDisplay(), provider.ForDisplay(),
					)
				}

				if provider.Hostname == addrs.DefaultProviderRegistryHost {
					suggestion += "\n\nIf you believe this provider is missing from the registry, please submit a issue on the OpenTofu Registry https://github.com/opentofu/registry/issues/new/choose"
				}

				warnDiags := warnOnFailedImplicitProvReference(provider, qualifs)
				diags = diags.Append(warnDiags)

				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to query available provider packages",
					fmt.Sprintf("Could not retrieve the list of available versions for provider %s: %s%s",
						provider.ForDisplay(), err, suggestion,
					),
				))
			case getproviders.ErrHostNoProviders:
				switch {
				case errorTy.Hostname == svchost.Hostname("github.com") && !errorTy.HasOtherVersion:
					// If a user copies the URL of a GitHub repository into
					// the source argument and removes the schema to make it
					// provider-address-shaped then that's one way we can end up
					// here. We'll use a specialized error message in anticipation
					// of that mistake. We only do this if github.com isn't a
					// provider registry, to allow for the (admittedly currently
					// rather unlikely) possibility that github.com starts being
					// a real Terraform provider registry in the future.
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						"Invalid provider registry host",
						fmt.Sprintf("The given source address %q specifies a GitHub repository rather than a OpenTofu provider. Refer to the documentation of the provider to find the correct source address to use.",
							provider.String(),
						),
					))

				case errorTy.HasOtherVersion:
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						"Invalid provider registry host",
						fmt.Sprintf("The host %q given in provider source address %q does not offer a OpenTofu provider registry that is compatible with this OpenTofu version, but it may be compatible with a different OpenTofu version.",
							errorTy.Hostname, provider.String(),
						),
					))

				default:
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						"Invalid provider registry host",
						fmt.Sprintf("The host %q given in provider source address %q does not offer a OpenTofu provider registry.",
							errorTy.Hostname, provider.String(),
						),
					))
				}

			case getproviders.ErrRequestCanceled:
				// We don't attribute cancellation to any particular operation,
				// but rather just emit a single general message about it at
				// the end, by checking ctx.Err().

			default:
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to resolve provider packages",
					fmt.Sprintf("Could not resolve provider %s: %s",
						provider.ForDisplay(), err,
					),
				))
			}

		},
		QueryPackagesWarning: func(provider addrs.Provider, warnings []string) {
			displayWarnings := make([]string, len(warnings))
			for i, warning := range warnings {
				displayWarnings[i] = fmt.Sprintf("- %s", warning)
			}

			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Additional provider information from registry",
				fmt.Sprintf("The remote registry returned warnings for %s:\n%s",
					provider.String(),
					strings.Join(displayWarnings, "\n"),
				),
			))
		},
		LinkFromCacheFailure: func(provider addrs.Provider, version getproviders.Version, err error) {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to install provider from shared cache",
				fmt.Sprintf("Error while importing %s v%s from the shared cache directory: %s.", provider.ForDisplay(), version, err),
			))
		},
		FetchPackageFailure: func(provider addrs.Provider, version getproviders.Version, err error) {
			const summaryIncompatible = "Incompatible provider version"
			switch err := err.(type) {
			case getproviders.ErrProtocolNotSupported:
				closestAvailable := err.Suggestion
				switch {
				case closestAvailable == getproviders.UnspecifiedVersion:
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						summaryIncompatible,
						fmt.Sprintf(errProviderVersionIncompatible, provider.String()),
					))
				case version.GreaterThan(closestAvailable):
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						summaryIncompatible,
						fmt.Sprintf(providerProtocolTooNew, provider.ForDisplay(),
							version, tfversion.String(), closestAvailable, closestAvailable,
							getproviders.VersionConstraintsString(reqs[provider]),
						),
					))
				default: // version is less than closestAvailable
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						summaryIncompatible,
						fmt.Sprintf(providerProtocolTooOld, provider.ForDisplay(),
							version, tfversion.String(), closestAvailable, closestAvailable,
							getproviders.VersionConstraintsString(reqs[provider]),
						),
					))
				}
			case getproviders.ErrPlatformNotSupported:
				switch {
				case err.MirrorURL != nil:
					// If we're installing from a mirror then it may just be
					// the mirror lacking the package, rather than it being
					// unavailable from upstream.
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						summaryIncompatible,
						fmt.Sprintf(
							"Your chosen provider mirror at %s does not have a %s v%s package available for your current platform, %s.\n\nProvider releases are separate from OpenTofu CLI releases, so this provider might not support your current platform. Alternatively, the mirror itself might have only a subset of the plugin packages available in the origin registry, at %s.",
							err.MirrorURL, err.Provider, err.Version, err.Platform,
							err.Provider.Hostname,
						),
					))
				default:
					diags = diags.Append(tfdiags.Sourceless(
						tfdiags.Error,
						summaryIncompatible,
						fmt.Sprintf(
							"Provider %s v%s does not have a package available for your current platform, %s.\n\nProvider releases are separate from OpenTofu CLI releases, so not all providers are available for all platforms. Other versions of this provider may have different platforms supported.",
							err.Provider, err.Version, err.Platform,
						),
					))
				}

			case getproviders.ErrRequestCanceled:
				// We don't attribute cancellation to any particular operation,
				// but rather just emit a single general message about it at
				// the end, by checking ctx.Err().

			default:
				// We can potentially end up in here under cancellation too,
				// in spite of our getproviders.ErrRequestCanceled case above,
				// because not all of the outgoing requests we do under the
				// "fetch package" banner are source metadata requests.
				// In that case we will emit a redundant error here about
				// the request being cancelled, but we'll still detect it
				// as a cancellation after the installer returns and do the
				// normal cancellation handling.

				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to install provider",
					fmt.Sprintf("Error while installing %s v%s: %s", provider.ForDisplay(), version, err),
				))
			}
		},
		FetchPackageSuccess: func(provider addrs.Provider, version getproviders.Version, localDir string, authResult *getproviders.PackageAuthenticationResult) {
			var keyID string
			if authResult != nil && authResult.Signed() {
				keyID = authResult.GPGKeyIDsString()
			}
			if keyID != "" {
				keyID = c.Colorize().Color(fmt.Sprintf(", key ID [reset][bold]%s[reset]", keyID))
			}

			if authResult != nil && authResult.SigningSkipped() {
				c.Ui.Warn(fmt.Sprintf("- Installed %s v%s. Signature validation was skipped due to the registry not containing GPG keys for this provider", provider.ForDisplay(), version))
			} else {
				c.Ui.Info(fmt.Sprintf("- Installed %s v%s (%s%s)", provider.ForDisplay(), version, authResult, keyID))
			}
		},
		CacheDirLockContended: func(cacheDir string) {
			c.Ui.Info(fmt.Sprintf("- Waiting for lock on cache directory %s", cacheDir))
		},
		ProvidersLockUpdated: func(provider addrs.Provider, version getproviders.Version, localHashes []getproviders.Hash, signedHashes []getproviders.Hash, priorHashes []getproviders.Hash) {
			// We're going to use this opportunity to track if we have any
			// "incomplete" installs of providers. An incomplete install is
			// when we are only going to write the local hashes into our lock
			// file which means a `tofu init` command will fail in future
			// when used on machines of a different architecture.
			//
			// We want to print a warning about this.

			if len(signedHashes) > 0 {
				// If we have any signedHashes hashes then we don't worry - as
				// we know we retrieved all available hashes for this version
				// anyway.
				return
			}

			// If local hashes and prior hashes are exactly the same then
			// it means we didn't record any signed hashes previously, and
			// we know we're not adding any extra in now (because we already
			// checked the signedHashes), so that's a problem.
			//
			// In the actual check here, if we have any priorHashes and those
			// hashes are not the same as the local hashes then we're going to
			// accept that this provider has been configured correctly.
			if len(priorHashes) > 0 && !reflect.DeepEqual(localHashes, priorHashes) {
				return
			}

			// Now, either signedHashes is empty, or priorHashes is exactly the
			// same as our localHashes which means we never retrieved the
			// signedHashes previously.
			//
			// Either way, this is bad. Let's complain/warn.
			incompleteProviders = append(incompleteProviders, provider.ForDisplay())
		},
		ProvidersAuthenticated: func(authResults map[addrs.Provider]*getproviders.PackageAuthenticationResult) {
			thirdPartySigned := false
			for _, authResult := range authResults {
				if authResult.Signed() {
					thirdPartySigned = true
					break
				}
			}
			if thirdPartySigned {
				c.Ui.Info(fmt.Sprintf("\nProviders are signed by their developers.\n" +
					"If you'd like to know more about provider signing, you can read about it here:\n" +
					"https://opentofu.org/docs/cli/plugins/signing/"))
			}
		},
	}
	ctx = evts.OnContext(ctx)

	mode := providercache.InstallNewProvidersOnly
	if upgrade {
		if flagLockfile == "readonly" {
			c.Ui.Error("The -upgrade flag conflicts with -lockfile=readonly.")
			return true, true, diags
		}

		mode = providercache.InstallUpgrades
	}
	newLocks, err := inst.EnsureProviderVersions(ctx, previousLocks, reqs, mode)
	if ctx.Err() == context.Canceled {
		c.showDiagnostics(diags)
		c.Ui.Error("Provider installation was canceled by an interrupt signal.")
		return true, true, diags
	}
	if err != nil {
		// The errors captured in "err" should be redundant with what we
		// received via the InstallerEvents callbacks above, so we'll
		// just return those as long as we have some.
		if !diags.HasErrors() {
			diags = diags.Append(err)
		}

		return true, true, diags
	}

	// If the provider dependencies have changed since the last run then we'll
	// say a little about that in case the reader wasn't expecting a change.
	// (When we later integrate module dependencies into the lock file we'll
	// probably want to refactor this so that we produce one lock-file related
	// message for all changes together, but this is here for now just because
	// it's the smallest change relative to what came before it, which was
	// a hidden JSON file specifically for tracking providers.)
	if !newLocks.Equal(previousLocks) {
		// if readonly mode
		if flagLockfile == "readonly" {
			// check if required provider dependencies change
			if !newLocks.EqualProviderAddress(previousLocks) {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					`Provider dependency changes detected`,
					`Changes to the required provider dependencies were detected, but the lock file is read-only. To use and record these requirements, run "tofu init" without the "-lockfile=readonly" flag.`,
				))
				return true, true, diags
			}

			// suppress updating the file to record any new information it learned,
			// such as a hash using a new scheme.
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				`Provider lock file not updated`,
				`Changes to the provider selections were detected, but not saved in the .terraform.lock.hcl file. To record these selections, run "tofu init" without the "-lockfile=readonly" flag.`,
			))
			return true, false, diags
		}

		// Jump in here and add a warning if any of the providers are incomplete.
		if len(incompleteProviders) > 0 {
			// We don't really care about the order here, we just want the
			// output to be deterministic.
			sort.Slice(incompleteProviders, func(i, j int) bool {
				return incompleteProviders[i] < incompleteProviders[j]
			})
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				incompleteLockFileInformationHeader,
				fmt.Sprintf(
					incompleteLockFileInformationBody,
					strings.Join(incompleteProviders, "\n  - "),
					getproviders.CurrentPlatform.String())))
		}

		if previousLocks.Empty() {
			// A change from empty to non-empty is special because it suggests
			// we're running "tofu init" for the first time against a
			// new configuration. In that case we'll take the opportunity to
			// say a little about what the dependency lock file is, for new
			// users or those who are upgrading from a previous Terraform
			// version that didn't have dependency lock files.
			c.Ui.Output(c.Colorize().Color(`
OpenTofu has created a lock file [bold].terraform.lock.hcl[reset] to record the provider
selections it made above. Include this file in your version control repository
so that OpenTofu can guarantee to make the same selections by default when
you run "tofu init" in the future.`))
		} else {
			c.Ui.Output(c.Colorize().Color(`
OpenTofu has made some changes to the provider dependency selections recorded
in the .terraform.lock.hcl file. Review those changes and commit them to your
version control system if they represent changes you intended to make.`))
		}

		moreDiags = c.replaceLockedDependencies(ctx, newLocks)
		diags = diags.Append(moreDiags)
	}

	return true, false, diags
}

// warnOnFailedImplicitProvReference returns a warn diagnostic when the downloader fails to fetch a provider that is implicitly referenced.
// In other words, if the failed to download provider is having no required_providers entry, this function is trying to give to the user
// more information on the source of the issue and gives also instructions on how to fix it.
func warnOnFailedImplicitProvReference(provider addrs.Provider, qualifs *getproviders.ProvidersQualification) tfdiags.Diagnostics {
	if _, ok := qualifs.Explicit[provider]; ok {
		return nil
	}
	refs, ok := qualifs.Implicit[provider]
	if !ok || len(refs) == 0 {
		// If there is no implicit reference for that provider, do not write the warn, let just the error to be returned.
		return nil
	}

	// NOTE: if needed, in the future we can use the rest of the "refs" to print all the culprits or at least to give
	// a hint on how many resources are causing this
	ref := refs[0]
	if ref.ProviderAttribute {
		return nil
	}
	details := fmt.Sprintf(
		implicitProviderReferenceBody,
		ref.CfgRes.String(),
		provider.Type,
		provider.ForDisplay(),
		provider.Type,
		ref.CfgRes.Resource.Type,
		provider.Type,
	)
	return tfdiags.Diagnostics{}.Append(
		&hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Subject:  ref.Ref.ToHCL().Ptr(),
			Summary:  implicitProviderReferenceHead,
			Detail:   details,
		})
}

// backendConfigOverrideBody interprets the raw values of -backend-config
// arguments into a hcl Body that should override the backend settings given
// in the configuration.
//
// If the result is nil then no override needs to be provided.
//
// If the returned diagnostics contains errors then the returned body may be
// incomplete or invalid.
func (c *InitCommand) backendConfigOverrideBody(flags rawFlags, schema *configschema.Block) (hcl.Body, tfdiags.Diagnostics) {
	items := flags.AllItems()
	if len(items) == 0 {
		return nil, nil
	}

	var ret hcl.Body
	var diags tfdiags.Diagnostics
	synthVals := make(map[string]cty.Value)

	mergeBody := func(newBody hcl.Body) {
		if ret == nil {
			ret = newBody
		} else {
			ret = configs.MergeBodies(ret, newBody)
		}
	}
	flushVals := func() {
		if len(synthVals) == 0 {
			return
		}
		newBody := configs.SynthBody("-backend-config=...", synthVals)
		mergeBody(newBody)
		synthVals = make(map[string]cty.Value)
	}

	if len(items) == 1 && items[0].Value == "" {
		// Explicitly remove all -backend-config options.
		// We do this by setting an empty but non-nil ConfigOverrides.
		return configs.SynthBody("-backend-config=''", synthVals), diags
	}

	for _, item := range items {
		eq := strings.Index(item.Value, "=")

		if eq == -1 {
			// The value is interpreted as a filename.
			newBody, fileDiags := c.loadHCLFile(item.Value)
			diags = diags.Append(fileDiags)
			if fileDiags.HasErrors() {
				continue
			}
			// Generate an HCL body schema for the backend block.
			var bodySchema hcl.BodySchema
			for name := range schema.Attributes {
				// We intentionally ignore the `Required` attribute here
				// because backend config override files can be partial. The
				// goal is to make sure we're not loading a file with
				// extraneous attributes or blocks.
				bodySchema.Attributes = append(bodySchema.Attributes, hcl.AttributeSchema{
					Name: name,
				})
			}
			for name, block := range schema.BlockTypes {
				var labelNames []string
				if block.Nesting == configschema.NestingMap {
					labelNames = append(labelNames, "key")
				}
				bodySchema.Blocks = append(bodySchema.Blocks, hcl.BlockHeaderSchema{
					Type:       name,
					LabelNames: labelNames,
				})
			}
			// Verify that the file body matches the expected backend schema.
			_, schemaDiags := newBody.Content(&bodySchema)
			diags = diags.Append(schemaDiags)
			if schemaDiags.HasErrors() {
				continue
			}
			flushVals() // deal with any accumulated individual values first
			mergeBody(newBody)
		} else {
			name := item.Value[:eq]
			rawValue := item.Value[eq+1:]
			attrS := schema.Attributes[name]
			if attrS == nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Invalid backend configuration argument",
					fmt.Sprintf("The backend configuration argument %q given on the command line is not expected for the selected backend type.", name),
				))
				continue
			}
			value, valueDiags := configValueFromCLI(item.String(), rawValue, attrS.Type)
			diags = diags.Append(valueDiags)
			if valueDiags.HasErrors() {
				continue
			}
			synthVals[name] = value
		}
	}

	flushVals()

	return ret, diags
}

func (c *InitCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictDirs("")
}

func (c *InitCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-backend":        completePredictBoolean,
		"-cloud":          completePredictBoolean,
		"-backend-config": complete.PredictFiles("*.tfvars"), // can also be key=value, but we can't "predict" that
		"-force-copy":     complete.PredictNothing,
		"-from-module":    completePredictModuleSource,
		"-get":            completePredictBoolean,
		"-input":          completePredictBoolean,
		"-lock":           completePredictBoolean,
		"-lock-timeout":   complete.PredictAnything,
		"-no-color":       complete.PredictNothing,
		"-plugin-dir":     complete.PredictDirs(""),
		"-reconfigure":    complete.PredictNothing,
		"-migrate-state":  complete.PredictNothing,
		"-upgrade":        completePredictBoolean,
	}
}

func (c *InitCommand) Help() string {
	helpText := `
Usage: tofu [global options] init [options]

  Initialize a new or existing OpenTofu working directory by creating
  initial files, loading any remote state, downloading modules, etc.

  This is the first command that should be run for any new or existing
  OpenTofu configuration per machine. This sets up all the local data
  necessary to run OpenTofu that is typically not committed to version
  control.

  This command is always safe to run multiple times. Though subsequent runs
  may give errors, this command will never delete your configuration or
  state. Even so, if you have important information, please back it up prior
  to running this command, just in case.

Options:

  -backend=false          Disable backend or cloud backend initialization
                          for this configuration and use what was previously
                          initialized instead.

                          aliases: -cloud=false

  -backend-config=path    Configuration to be merged with what is in the
                          configuration file's 'backend' block. This can be
                          either a path to an HCL file with key/value
                          assignments (same format as terraform.tfvars) or a
                          'key=value' format, and can be specified multiple
                          times. The backend type must be in the configuration
                          itself.

  -compact-warnings       If OpenTofu produces any warnings that are not
                          accompanied by errors, show them in a more compact
                          form that includes only the summary messages.

  -consolidate-warnings   If OpenTofu produces any warnings, no consolidation
                          will be performed. All locations, for all warnings
                          will be listed. Enabled by default.

  -consolidate-errors     If OpenTofu produces any errors, no consolidation
                          will be performed. All locations, for all errors
                          will be listed. Disabled by default

  -force-copy             Suppress prompts about copying state data when
                          initializing a new state backend. This is
                          equivalent to providing a "yes" to all confirmation
                          prompts.

  -from-module=SOURCE     Copy the contents of the given module into the target
                          directory before initialization.

  -get=false              Disable downloading modules for this configuration.

  -input=false            Disable interactive prompts. Note that some actions may
                          require interactive prompts and will error if input is
                          disabled.

  -lock=false             Don't hold a state lock during backend migration.
                          This is dangerous if others might concurrently run
                          commands against the same workspace.

  -lock-timeout=0s        Duration to retry a state lock.

  -no-color               If specified, output won't contain any color.

  -plugin-dir             Directory containing plugin binaries. This overrides all
                          default search paths for plugins, and prevents the
                          automatic installation of plugins. This flag can be used
                          multiple times.

  -reconfigure            Reconfigure a backend, ignoring any saved
                          configuration.

  -migrate-state          Reconfigure a backend, and attempt to migrate any
                          existing state.

  -upgrade                Install the latest module and provider versions
                          allowed within configured constraints, overriding the
                          default behavior of selecting exactly the version
                          recorded in the dependency lockfile.

  -lockfile=MODE          Set a dependency lockfile mode.
                          Currently only "readonly" is valid.

  -ignore-remote-version  A rare option used for cloud backend and the remote backend
                          only. Set this to ignore checking that the local and remote
                          OpenTofu versions use compatible state representations, making
                          an operation proceed even when there is a potential mismatch.
                          See the documentation on configuring OpenTofu with
                          cloud backend for more information.

  -test-directory=path    Set the OpenTofu test directory, defaults to "tests". When set, the
                          test command will search for test files in the current directory and
                          in the one specified by the flag.

  -json                   Produce output in a machine-readable JSON format, 
                          suitable for use in text editor integrations and other 
                          automated systems. Always disables color.

  -var 'foo=bar'          Set a value for one of the input variables in the root
                          module of the configuration. Use this option more than
                          once to set more than one variable.

  -var-file=filename      Load variable values from the given file, in addition
                          to the default files terraform.tfvars and *.auto.tfvars.
                          Use this option more than once to include more than one
                          variables file.

`
	return strings.TrimSpace(helpText)
}

func (c *InitCommand) Synopsis() string {
	return "Prepare your working directory for other commands"
}

const errInitConfigError = `
[reset]OpenTofu encountered problems during initialization, including problems
with the configuration, described below.

The OpenTofu configuration must be valid before initialization so that
OpenTofu can determine which modules and providers need to be installed.
`

const errInitCopyNotEmpty = `
The working directory already contains files. The -from-module option requires
an empty directory into which a copy of the referenced module will be placed.

To initialize the configuration already in this working directory, omit the
-from-module option.
`

const outputInitEmpty = `
[reset][bold]OpenTofu initialized in an empty directory![reset]

The directory has no OpenTofu configuration files. You may begin working
with OpenTofu immediately by creating OpenTofu configuration files.
`

const outputInitSuccess = `
[reset][bold][green]OpenTofu has been successfully initialized![reset][green]
`

const outputInitSuccessCloud = `
[reset][bold][green]Cloud backend has been successfully initialized![reset][green]
`

const outputInitSuccessCLI = `[reset][green]
You may now begin working with OpenTofu. Try running "tofu plan" to see
any changes that are required for your infrastructure. All OpenTofu commands
should now work.

If you ever set or change modules or backend configuration for OpenTofu,
rerun this command to reinitialize your working directory. If you forget, other
commands will detect it and remind you to do so if necessary.
`

const outputInitSuccessCLICloud = `[reset][green]
You may now begin working with cloud backend. Try running "tofu plan" to
see any changes that are required for your infrastructure.

If you ever set or change modules or OpenTofu Settings, run "tofu init"
again to reinitialize your working directory.
`

// providerProtocolTooOld is a message sent to the CLI UI if the provider's
// supported protocol versions are too old for the user's version of tofu,
// but a newer version of the provider is compatible.
const providerProtocolTooOld = `Provider %q v%s is not compatible with OpenTofu %s.
Provider version %s is the latest compatible version. Select it with the following version constraint:
	version = %q

OpenTofu checked all of the plugin versions matching the given constraint:
	%s

Consult the documentation for this provider for more information on compatibility between provider and OpenTofu versions.
`

// providerProtocolTooNew is a message sent to the CLI UI if the provider's
// supported protocol versions are too new for the user's version of tofu,
// and the user could either upgrade tofu or choose an older version of the
// provider.
const providerProtocolTooNew = `Provider %q v%s is not compatible with OpenTofu %s.
You need to downgrade to v%s or earlier. Select it with the following constraint:
	version = %q

OpenTofu checked all of the plugin versions matching the given constraint:
	%s

Consult the documentation for this provider for more information on compatibility between provider and OpenTofu versions.
Alternatively, upgrade to the latest version of OpenTofu for compatibility with newer provider releases.
`

// No version of the provider is compatible.
const errProviderVersionIncompatible = `No compatible versions of provider %s were found.`

// incompleteLockFileInformationHeader is the summary displayed to users when
// the lock file has only recorded local hashes.
const incompleteLockFileInformationHeader = `Incomplete lock file information for providers`

// incompleteLockFileInformationBody is the body of text displayed to users when
// the lock file has only recorded local hashes.
const incompleteLockFileInformationBody = `Due to your customized provider installation methods, OpenTofu was forced to calculate lock file checksums locally for the following providers:
  - %s

The current .terraform.lock.hcl file only includes checksums for %s, so OpenTofu running on another platform will fail to install these providers.

To calculate additional checksums for another platform, run:
  tofu providers lock -platform=linux_amd64
(where linux_amd64 is the platform to generate)`

const implicitProviderReferenceHead = `Automatically-inferred provider dependency`

const implicitProviderReferenceBody = `Due to the prefix of the resource type name OpenTofu guessed that you intended to associate %s with a provider whose local name is "%s", but that name is not declared in this module's required_providers block. OpenTofu therefore guessed that you intended to use %s, but that provider does not exist.

Make at least one of the following changes to tell OpenTofu which provider to use:

- Add a declaration for local name "%s" to this module's required_providers block, specifying the full source address for the provider you intended to use.
- Verify that "%s" is the correct resource type name to use. Did you omit a prefix which would imply the correct provider?
- Use a "provider" argument within this resource block to override OpenTofu's automatic selection of the local name "%s".
`
