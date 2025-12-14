package output

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// OutputTable prints a simple tab-aligned table to stderr (human mode).
func OutputTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	_ = w.Flush()
}

// OutputList prints one item per line to stderr (human mode).
func OutputList(items []string) {
	for _, item := range items {
		fmt.Fprintln(os.Stderr, item)
	}
}

