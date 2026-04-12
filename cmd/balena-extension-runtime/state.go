package main

import (
	"encoding/json"
	"fmt"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/spf13/cobra"
)

var stateCmd = &cobra.Command{
	Use:   "state <container-id>",
	Short: "Get the state of an extension container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := oci.ReadState(args[0])
		if err != nil {
			return fmt.Errorf("failed to read state: %w", err)
		}

		output, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("failed to marshal state: %w", err)
		}

		fmt.Println(string(output))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stateCmd)
}
