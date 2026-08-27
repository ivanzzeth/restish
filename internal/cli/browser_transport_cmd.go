package cli

import (
	"github.com/spf13/cobra"
)

// browserTransportMarker identifies builds carrying the open-surface browser
// transport layer. Generated CLIs probe for it so they fail loudly instead of
// silently falling back to a stock restish (which ignores the profile's
// `browser: true` and issues bare HTTP that anti-bot layers reject).
const browserTransportMarker = "open-surface-browser-transport"

func (c *CLI) addBrowserTransportCommand(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:    "browser-transport",
		Short:  "Print the open-surface browser transport marker",
		Hidden: true,
		Args:   usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.Println(browserTransportMarker)
			return nil
		},
	})
}
