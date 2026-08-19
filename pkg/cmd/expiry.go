package cmd

import (
	"fmt"

	"github.com/dedalus-labs/dedalus-cli/internal/requestflag"
	"github.com/urfave/cli/v3"
)

func init() {
	if err := configureExpiryFlags(Command); err != nil {
		panic(err)
	}
}

// configureExpiryFlags exposes every generated expires_in field as --expires
// with the shared duration parser while keeping integer seconds on the wire.
func configureExpiryFlags(command *cli.Command) error {
	for index, flag := range command.Flags {
		names := flag.Names()
		if len(names) == 0 || names[0] != "expires-in" {
			continue
		}

		switch expiry := flag.(type) {
		case *requestflag.Flag[int64]:
			expiry.Name = "expires"
			command.Flags[index] = requestflag.NewDurationSecondsFlag(expiry)
		case *requestflag.DurationSecondsFlag:
			continue
		default:
			return fmt.Errorf(
				"command %q has expires_in with unexpected generated type %T",
				command.Name, flag,
			)
		}
	}

	for _, child := range command.Commands {
		if err := configureExpiryFlags(child); err != nil {
			return err
		}
	}
	return nil
}
