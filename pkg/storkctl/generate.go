package storkctl

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func newGenerateCommand(cmdFactory Factory, ioStreams genericclioptions.IOStreams) *cobra.Command {
	generateCommands := &cobra.Command{
		Use:        "generate",
		Short:      "Generate stork specs",
		Deprecated: "use command \"create\" instead.",
	}

	generateCommands.AddCommand(
		newGenerateClusterPairCommand(cmdFactory, ioStreams),
	)

	return generateCommands
}
