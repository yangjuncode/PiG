// 中文导读：包管理器内部实现——install_options。
package packagemanager

// InstallOption adjusts how one managed install runs its package manager. With no option an install behaves as Pi's does.
type InstallOption func(*installOptions)

type installOptions struct {
	ignoreScripts bool
}

// WithIgnoreScripts runs the package manager with `--ignore-scripts`, so no lifecycle script of the installed Package or of its dependencies runs during the install. Npm, pnpm and bun accept the same flag.
//
// pig additive (D194): Piglet resolution installs Packages without running lifecycle scripts unless the user opts in.
func WithIgnoreScripts() InstallOption {
	return func(options *installOptions) { options.ignoreScripts = true }
}

func resolveInstallOptions(opts []InstallOption) installOptions {
	var options installOptions
	for _, opt := range opts {
		opt(&options)
	}
	return options
}

// ignoreScriptsArgs is the argument that every supported package manager reads as "run no lifecycle scripts".
func (options installOptions) ignoreScriptsArgs() []string {
	if options.ignoreScripts {
		return []string{"--ignore-scripts"}
	}
	return nil
}
