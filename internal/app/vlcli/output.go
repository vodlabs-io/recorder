package vlcli

import (
	"encoding/json"
	"os"
	"text/tabwriter"

	"github.com/urfave/cli/v3"
)

// jsonFlag is carried by every read command. The daemon answers in JSON and the
// CLI reformats it into aligned columns that nothing can parse back; --json
// hands the decoded payload over unchanged so a script does not have to scrape
// the table.
func jsonFlag() cli.Flag {
	return &cli.BoolFlag{
		Name:  "json",
		Usage: "Print the result as JSON instead of a table",
	}
}

// emitJSON writes v as indented JSON on stdout.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// newTable returns a tabwriter configured the way every listing in this CLI
// uses one, so columns line up the same from command to command.
func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

// dash renders an empty value as a placeholder, so a blank column reads as
// "nothing here" rather than as a broken layout.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// yesNo renders a bool for a table column.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
