package clankcli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestPullMigrateConflict_PromotesDiscardLocal pins the styled
// options block's new wording: --discard-local is the primary
// suggestion, with --force kept visible as a legacy alias. Symmetric
// to the push side's --discard-remote promotion.
func TestPullMigrateConflict_PromotesDiscardLocal(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	cmd.SetOut(stdout)

	printPullMigrateConflict(cmd)
	out := stripANSI(stdout.String())
	for _, want := range []string{
		"Cannot migrate",
		"clank push -m",
		"clank pull -m --discard-local",
		"legacy alias",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in conflict block:\n%s", want, out)
		}
	}
}

// TestPullCmd_DiscardLocalRequiresMigrate and ...ForceAliasRequiresMigrate
// pin the alias wiring: BOTH --discard-local and --force funnel through
// the same validation gate. If the alias were misregistered (different
// variable, missing fold-in) only one of the two would error here.
func TestPullCmd_DiscardLocalRequiresMigrate(t *testing.T) {
	t.Parallel()
	assertDiscardOnlyApplies(t, "--discard-local")
}

func TestPullCmd_ForceAliasRequiresMigrate(t *testing.T) {
	t.Parallel()
	assertDiscardOnlyApplies(t, "--force")
}

func assertDiscardOnlyApplies(t *testing.T, flag string) {
	t.Helper()
	cmd := pullCmd()
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{flag})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected validation error for %s without --migrate, got nil", flag)
	}
	if !strings.Contains(err.Error(), "--discard-local only applies with --migrate") {
		t.Errorf("expected canonical-name validation message for %s; got %v", flag, err)
	}
}

// TestPushCmd_DiscardRemoteRequiresMigrate and ...ForceAliasRequiresMigrate
// mirror the pull alias coverage on the push side.
func TestPushCmd_DiscardRemoteRequiresMigrate(t *testing.T) {
	t.Parallel()
	assertPushDiscardOnlyApplies(t, "--discard-remote")
}

func TestPushCmd_ForceAliasRequiresMigrate(t *testing.T) {
	t.Parallel()
	assertPushDiscardOnlyApplies(t, "--force")
}

func assertPushDiscardOnlyApplies(t *testing.T, flag string) {
	t.Helper()
	cmd := pushCmd()
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{flag})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected validation error for %s without --migrate, got nil", flag)
	}
	if !strings.Contains(err.Error(), "--discard-remote only applies with --migrate") {
		t.Errorf("expected canonical-name validation message for %s; got %v", flag, err)
	}
}
