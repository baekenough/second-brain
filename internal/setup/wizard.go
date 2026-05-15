//go:build setup

package setup

import (
	"fmt"
	"os"
	"strings"

	"charm.land/huh/v2"
)

// Run is the entry point invoked by cmd/collector/main.go when
// the first argument is "setup". args contains os.Args[2:].
//
// Flags:
//
//	--non-interactive   Run in accessible (plain-text) mode; useful for CI
//	                    or when there is no interactive terminal.
//	--output <path>     Write .env to <path> instead of ".env" (default).
func Run(args []string) error {
	nonInteractive := false
	outputPath := EnvFilePath()

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--non-interactive":
			nonInteractive = true
		case "--output":
			if i+1 >= len(args) {
				return fmt.Errorf("--output requires a path argument")
			}
			i++
			outputPath = args[i]
		}
	}

	if nonInteractive {
		fmt.Fprintln(os.Stderr, msg("non_interactive"))
	}

	// --- Channel selection ---
	channelIDs := make([]string, 0, len(Registry))
	var channelOptions []huh.Option[string]
	for _, ch := range Registry {
		channelOptions = append(channelOptions, huh.NewOption(ch.Label, ch.ID))
	}

	channelForm := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title(msg("welcome")).
				Description(msg("welcome_desc")),
			huh.NewMultiSelect[string]().
				Title(msg("channel_title")).
				Description(msg("channel_desc")).
				Options(channelOptions...).
				Value(&channelIDs),
		),
	).WithAccessible(nonInteractive)

	if err := channelForm.Run(); err != nil {
		if isAbort(err) {
			fmt.Fprintln(os.Stderr, msg("aborted"))
			return nil
		}
		return fmt.Errorf("channel selection: %w", err)
	}

	if len(channelIDs) == 0 {
		fmt.Fprintln(os.Stderr, msg("aborted"))
		return nil
	}

	// Build a set for fast lookup.
	selected := make(map[string]bool, len(channelIDs))
	for _, id := range channelIDs {
		selected[id] = true
	}

	// --- Per-channel prompts ---
	pairs := make(map[string]string)
	var order []string

	for _, ch := range Registry {
		if !selected[ch.ID] {
			continue
		}

		if ch.InfoOnly {
			fmt.Fprintf(os.Stderr, "%s %s\n", msg("info_only_prefix"), ch.InfoMessage)
			continue
		}

		var fields []huh.Field
		values := make(map[string]*string, len(ch.Vars))

		for _, v := range ch.Vars {
			if v.Hardcoded {
				pairs[v.Key] = v.DefaultValue
				order = append(order, v.Key)
				continue
			}

			val := v.DefaultValue
			values[v.Key] = &val

			if v.Multiline {
				if v.Secret {
					fmt.Fprintf(os.Stderr, "⚠ %s: input will be visible while pasting (multiline secret cannot be masked). Paste in a private terminal.\n", v.Key)
				}
				f := huh.NewText().
					Title(v.Label).
					Description(v.Description).
					Value(values[v.Key])
				fields = append(fields, f)
			} else {
				f := huh.NewInput().
					Title(v.Label).
					Description(v.Description).
					Value(values[v.Key])
				if v.Secret {
					f = f.EchoMode(huh.EchoModePassword)
				}
				fields = append(fields, f)
			}
			order = append(order, v.Key)
		}

		if len(fields) > 0 {
			g := huh.NewGroup(fields...).Title(ch.Label)
			form := huh.NewForm(g).WithAccessible(nonInteractive)
			if err := form.Run(); err != nil {
				if isAbort(err) {
					fmt.Fprintln(os.Stderr, msg("aborted"))
					return nil
				}
				return fmt.Errorf("channel %s: %w", ch.ID, err)
			}

			for key, valPtr := range values {
				v := strings.TrimSpace(*valPtr)
				if v != "" {
					pairs[key] = v
				}
			}
		}
	}

	if len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, msg("aborted"))
		return nil
	}

	// --- Write .env ---
	if err := WriteEnv(outputPath, pairs, order); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}

	fmt.Fprintf(os.Stdout, msg("written")+"\n", outputPath)
	fmt.Fprintln(os.Stdout, msg("done"))
	return nil
}

// isAbort returns true when the error represents a user-initiated abort
// (Ctrl+C / Ctrl+D) rather than a genuine failure.
func isAbort(err error) bool {
	if err == nil {
		return false
	}
	return err == huh.ErrUserAborted ||
		strings.Contains(err.Error(), "aborted") ||
		strings.Contains(err.Error(), "interrupt")
}
