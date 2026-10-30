package ui

import "fmt"

// dialogBodyRow is one wrapped display row of a dialog's body; index is the
// body entry it came from (the first entry of a permission prompt — the
// command, the path — is drawn at full contrast).
type dialogBodyRow struct {
	text  string
	index int
}

// dialogBodyChrome is the rows a dialog spends besides its body: title, the
// blank before the options, borders, hint, padding and a little room for the
// footer the dialog sits above.
const dialogBodyChrome = 9

// dialogBodyBudget is how many body rows fit a dialog on a screen of height
// rows: everything the screen has left after the dialog's options and chrome.
// 0 means no limit (unknown height).
func dialogBodyBudget(d *Dialog, height int) int {
	if height <= 0 || d == nil {
		return 0
	}
	return max(height-len(d.Options)-dialogBodyChrome, 3)
}

// dialogBodyRows wraps a dialog's body to inner cells — never truncating a
// line, so the end of a long command stays visible — and, when the rows
// exceed maxRows (> 0), keeps the first maxRows-1 and returns a notice naming
// how many rows are not shown. A permission prompt must never look complete
// when it is not: the hidden part of a command is where a payload would go.
func dialogBodyRows(d *Dialog, inner, maxRows int) (rows []dialogBodyRow, notice string) {
	if d == nil {
		return nil, ""
	}
	for i, b := range d.Body {
		for _, ln := range wrapCells(b, inner) {
			rows = append(rows, dialogBodyRow{text: ln, index: i})
		}
	}
	if maxRows > 0 && len(rows) > maxRows {
		keep := max(maxRows-1, 1)
		hidden := len(rows) - keep
		rows = rows[:keep]
		notice = fmt.Sprintf("⚠ %d more line(s) of this request are not shown — the screen is too small to show it whole; choose No unless you have reviewed it", hidden)
	}
	return rows, notice
}
