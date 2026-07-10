// Package cmd assembles the orkano CLI from cobra commands. main wires it to the
// process; the command logic lives here so it is testable without a process.
package cmd

import "github.com/spf13/cobra"

// NewRootCommand builds the orkano root command tree for the given version.
func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "orkano",
		Short:         "Orkano — open-source, self-hosted PaaS on Kubernetes",
		Version:       version,
		SilenceUsage:  true, // a runtime error is not a usage error
		SilenceErrors: true, // main prints the error once
	}
	root.AddCommand(newInitCommand(version))
	root.AddCommand(newPreflightCommand())
	root.AddCommand(newDoctorCommand())
	return root
}
