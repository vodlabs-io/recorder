package vlcli

import (
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// looksLikeURL decides which field of a StreamerRef a single argument fills.
// Platform names are bare words -- "twitch", "kick" -- so anything carrying a
// scheme, a slash or a dot was meant as a watch page URL. This only picks the
// field; the daemon still does the real validation and resolution.
func looksLikeURL(s string) bool {
	return strings.Contains(s, "://") || strings.Contains(s, "/") || strings.Contains(s, ".")
}

// streamerRefFromArgs reads a streamer from a command's positional arguments in
// either accepted form: a watch page URL, or a platform and username. It
// validates before the caller does anything, so a malformed invocation fails
// before reaching the daemon.
func streamerRefFromArgs(cmd *cli.Command, path string) (streamer.StreamerRef, error) {
	// Declared Arguments are already bound, so anything left over is a word the
	// command has no use for. Silently ignoring it hides a typo.
	if err := rejectExtraArgs(cmd, 0, path); err != nil {
		return streamer.StreamerRef{}, err
	}
	first := strings.TrimSpace(cmd.StringArg("platform"))
	second := strings.TrimSpace(cmd.StringArg("username"))
	return streamerRef(first, second, path)
}

// streamerRef builds the ref from two already-extracted arguments, so callers
// with a different argument layout (variant) can reuse the same rules. tail is
// passed through to usageLine for a command that takes a further argument.
func streamerRef(first, second, path string, tail ...string) (streamer.StreamerRef, error) {
	switch {
	case first == "":
		return streamer.StreamerRef{}, usageError(path, tail...)
	case looksLikeURL(first):
		if second != "" {
			return streamer.StreamerRef{}, fmt.Errorf(
				"a URL names the streamer on its own; drop the extra %q\n%s", second, usageLine(path, tail...))
		}
		return streamer.StreamerRef{URL: first}, nil
	case second == "":
		return streamer.StreamerRef{}, fmt.Errorf(
			"missing username for platform %q\n%s", first, usageLine(path, tail...))
	default:
		return streamer.StreamerRef{
			StreamerKey: streamer.StreamerKey{Platform: first, Username: second},
		}, nil
	}
}

func usageError(path string, tail ...string) error {
	return fmt.Errorf("missing streamer\n%s", usageLine(path, tail...))
}

// usageLine spells out both accepted forms. tail is any argument the command
// takes after the streamer (only `variant` has one).
func usageLine(path string, tail ...string) string {
	suffix := ""
	if len(tail) > 0 && tail[0] != "" {
		suffix = " " + tail[0]
	}
	return fmt.Sprintf("usage: %s %s <url>%s\n   or: %s %s <platform> <username>%s",
		RootCommandName, path, suffix, RootCommandName, path, suffix)
}

// streamerArgs is the argument pair every streamer command takes. The names are
// the platform/username form; a lone URL lands in "platform" and is recognised
// by shape.
func streamerArgs() []cli.Argument {
	return []cli.Argument{
		&cli.StringArg{Name: "platform"},
		&cli.StringArg{Name: "username"},
	}
}

// streamerArgsUsage is the help line for that pair.
const streamerArgsUsage = "<url> | <platform> <username>"
