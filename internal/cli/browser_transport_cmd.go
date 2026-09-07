package cli

import (
	"fmt"

	"github.com/rest-sh/restish/v2/internal/request"
	"github.com/spf13/cobra"
)

// browserTransportMarker identifies builds carrying the open-surface browser
// transport layer. Generated CLIs probe for it so they fail loudly instead of
// silently falling back to a stock restish (which ignores the profile's
// `browser: true` and issues bare HTTP that anti-bot layers reject).
const browserTransportMarker = "open-surface-browser-transport"

func (c *CLI) addBrowserTransportCommand(root *cobra.Command) {
	var probe bool
	var target string
	command := &cobra.Command{
		Use:    "browser-transport",
		Short:  "Print the open-surface browser transport marker",
		Hidden: true,
		Args:   usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if probe {
				if target == "" {
					return fmt.Errorf("--target is required with --probe")
				}
				transport := request.NewBrowserRoundTripper(
					request.BrowserRoundTripperConfig{Target: target},
				)
				if err := transport.Probe(); err != nil {
					return err
				}
				cmd.Println("open-surface-browser-transport-ready")
				return nil
			}
			cmd.Println(browserTransportMarker)
			return nil
		},
	}
	command.Flags().BoolVar(&probe, "probe", false, "Start and health-check the policy forwarder without an upstream request")
	command.Flags().StringVar(&target, "target", "", "open-surface target used by the policy forwarder")
	root.AddCommand(command)
}
