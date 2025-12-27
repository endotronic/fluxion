package table

import (
	"fmt"
	"io"
	"strings"
)

// Table represents a simple ASCII table
type Table struct {
	headers []string
	rows    [][]string
}

// New creates a new Table with the given headers
func New(headers []string) *Table {
	return &Table{
		headers: headers,
		rows:    make([][]string, 0),
	}
}

// AddRow adds a row to the table. The length of row must match headers.
func (t *Table) AddRow(row []string) {
	if len(row) != len(t.headers) {
		// In a real app we might return error, but for CLI helper panic or ignore is often acceptable.
		// Let's just ignore or pad? Panic seems safer for dev feedback.
		// For robustness, let's just not crash.
		return
	}
	t.rows = append(t.rows, row)
}

// Print writes the formatted table to the writer
func (t *Table) Print(w io.Writer) {
	if len(t.headers) == 0 {
		return
	}

	// 1. Calculate widths
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = len(h)
	}

	for _, row := range t.rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// 2. Build Format String
	// e.g. "%-10s  %-20s  %-5s\n"
	var formatParts []string
	for _, width := range widths {
		formatParts = append(formatParts, fmt.Sprintf("%%-%ds", width))
	}
	formatStr := strings.Join(formatParts, "  ") + "\n" // 2 spaces spacing

	// 3. Print Header
	// We need []interface{} for Printf
	headerArgs := make([]interface{}, len(t.headers))
	for i, h := range t.headers {
		headerArgs[i] = h
	}
	fmt.Fprintf(w, formatStr, headerArgs...)

	// 4. Print Separator
	sepArgs := make([]interface{}, len(t.headers))
	for i, w := range widths {
		sepArgs[i] = strings.Repeat("-", w)
	}
	fmt.Fprintf(w, formatStr, sepArgs...)

	// 5. Print Rows
	for _, row := range t.rows {
		rowArgs := make([]interface{}, len(row))
		for i, cell := range row {
			rowArgs[i] = cell
		}
		fmt.Fprintf(w, formatStr, rowArgs...)
	}
}
