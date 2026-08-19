package cmd

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dedalus-labs/dedalus-cli/internal/requestflag"
	"github.com/urfave/cli/v3"
)

func TestExpiryFlagsAcceptDurationsAndSendIntegerSeconds(t *testing.T) {
	t.Parallel()

	portCreate := expiryTestCommand("create-port")
	keyMint := expiryTestCommand("mint-port-key")
	root := &cli.Command{
		Name: "dedalus",
		Commands: []*cli.Command{
			{Name: "machines:ports", Commands: []*cli.Command{portCreate}},
			{Name: "machines:ports:access-keys", Commands: []*cli.Command{keyMint}},
		},
	}
	if err := configureExpiryFlags(root); err != nil {
		t.Fatalf("configure expiry flags: %v", err)
	}

	for _, command := range []*cli.Command{portCreate, keyMint} {
		flag := command.Flags[0]
		if got, want := flag.Names(), []string{"expires"}; !slices.Equal(got, want) {
			t.Errorf("%s expiry flag names = %v, want %v", command.Name, got, want)
		}
		if err := flag.PreParse(); err != nil {
			t.Fatalf("preparse %s expiry: %v", command.Name, err)
		}
		if err := flag.Set("expires", "2d12h"); err != nil {
			t.Fatalf("set %s expiry: %v", command.Name, err)
		}

		body, ok := requestflag.ExtractRequestContents(command).Body.(map[string]any)
		if !ok {
			t.Fatalf("%s request body has unexpected type", command.Name)
		}
		if got, want := body["expires_in"], int64(216_000); got != want {
			t.Errorf("%s expires_in = %v, want %d", command.Name, got, want)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s request body: %v", command.Name, err)
		}
		if got, want := string(encoded), `{"expires_in":216000}`; got != want {
			t.Errorf("%s request body = %s, want %s", command.Name, got, want)
		}
		docFlag, ok := flag.(cli.DocGenerationFlag)
		if !ok {
			t.Fatalf("%s expiry flag does not support documentation", command.Name)
		}
		if got, want := docFlag.TypeName(), "duration"; got != want {
			t.Errorf("%s expiry flag type = %q, want %q", command.Name, got, want)
		}
		usage := docFlag.GetUsage()
		if !strings.Contains(usage, "30s, 30m, 2h, 7d3h4s, or 1w3d") ||
			!strings.Contains(usage, `raw seconds ("1800")`) {
			t.Errorf("%s expiry usage = %q, want autosleep duration examples", command.Name, usage)
		}
		if got := flag.String(); !strings.Contains(got, "duration") {
			t.Errorf("%s expiry help = %q, want duration type", command.Name, got)
		}
	}
}

func TestExpiryFlagsRejectUnexpectedGeneratedType(t *testing.T) {
	t.Parallel()

	root := &cli.Command{
		Name: "dedalus",
		Commands: []*cli.Command{{
			Name: "create",
			Flags: []cli.Flag{&requestflag.Flag[string]{
				Name:     "expires-in",
				BodyPath: "expires_in",
			}},
		}},
	}

	err := configureExpiryFlags(root)
	if err == nil {
		t.Fatal("configureExpiryFlags accepted a non-integer expires_in field")
	}
	if !strings.Contains(err.Error(), "unexpected generated type") {
		t.Errorf("configureExpiryFlags error = %q, want unexpected generated type", err)
	}
}

func expiryTestCommand(name string) *cli.Command {
	return &cli.Command{
		Name: name,
		Flags: []cli.Flag{&requestflag.Flag[int64]{
			Name:     "expires-in",
			BodyPath: "expires_in",
			Required: true,
		}},
	}
}
