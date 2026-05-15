//go:build setup

package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
)

// Run is the entry point invoked by cmd/collector/main.go when
// the first argument is "setup". args contains os.Args[2:].
//
// Flags:
//
//	--non-interactive        Run in accessible (plain-text) mode; useful for CI
//	                         or when there is no interactive terminal.
//	--output <path>          Write .env to <path> instead of ".env" (default).
//	--channels <id,id,...>   Comma-separated channel IDs to configure. Skips the
//	                         interactive multi-select form; useful for scripting.
//	--value KEY=VALUE        Pre-set a channel variable; may be repeated. Skips
//	                         the per-channel prompt for the given key.
func Run(args []string) error {
	nonInteractive := false
	outputPath := EnvFilePath()
	var channelFlag string                 // comma-separated channel IDs (--channels)
	presetValues := make(map[string]string) // KEY=VALUE pairs from --value flags

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
		case "--channels":
			if i+1 >= len(args) {
				return fmt.Errorf("--channels requires a comma-separated list of channel IDs")
			}
			i++
			channelFlag = args[i]
		case "--value":
			if i+1 >= len(args) {
				return fmt.Errorf("--value requires a KEY=VALUE argument")
			}
			i++
			kv := args[i]
			idx := strings.IndexByte(kv, '=')
			if idx < 1 {
				return fmt.Errorf("--value %q: expected KEY=VALUE format", kv)
			}
			presetValues[kv[:idx]] = kv[idx+1:]
		}
	}

	if err := validateOutputPath(outputPath); err != nil {
		return fmt.Errorf("invalid --output path: %w", err)
	}

	if nonInteractive {
		fmt.Fprintln(os.Stderr, msg("non_interactive"))
	}

	// --- Channel selection ---
	// When --channels is provided, skip the interactive multi-select form.
	var channelIDs []string
	if channelFlag != "" {
		for _, id := range strings.Split(channelFlag, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				channelIDs = append(channelIDs, id)
			}
		}
	} else {
		channelIDs = make([]string, 0, len(Registry))
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

			// If --value KEY=VALUE was supplied, skip the interactive prompt.
			if preset, ok := presetValues[v.Key]; ok {
				pairs[v.Key] = preset
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

// validateOutputPath rejects paths that escape the current working directory
// or point to sensitive system locations. Valid paths must be relative or
// resolve to a location inside the current working directory.
//
// Rejected:
//   - Absolute paths outside cwd (e.g. /etc/passwd, /tmp/x.env)
//   - Paths containing ".." that escape cwd
//   - Paths under /etc, /root, /usr, /bin, /sbin, /sys, /proc
func validateOutputPath(path string) error {
	// Reject paths with ".." components before any resolution to catch
	// obvious traversal attempts early.
	if strings.Contains(path, "..") {
		return fmt.Errorf("path %q must not contain '..'", path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot resolve path %q: %w", path, err)
	}

	// Deny well-known sensitive system directories regardless of cwd.
	sensitivePrefixes := []string{
		"/etc/", "/etc",
		"/root/", "/root",
		"/usr/", "/usr",
		"/bin/", "/bin",
		"/sbin/", "/sbin",
		"/sys/", "/sys",
		"/proc/", "/proc",
	}
	for _, prefix := range sensitivePrefixes {
		if abs == prefix || strings.HasPrefix(abs, prefix+"/") || abs == strings.TrimSuffix(prefix, "/") {
			return fmt.Errorf("path %q resolves to a restricted system location", path)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	// The resolved path must be inside the current working directory.
	if !strings.HasPrefix(abs, cwd+string(filepath.Separator)) && abs != cwd {
		return fmt.Errorf("path %q (resolved: %s) escapes the working directory %s", path, abs, cwd)
	}

	return nil
}
