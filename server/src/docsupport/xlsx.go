package docsupport

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// ParseXLSX extracts text from an XLSX byte slice.
// Each sheet is printed as "--- Sheet: Name ---" followed by tab-separated rows.
func ParseXLSX(data []byte, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 10000
	}

	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("opening XLSX: %w", err)
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", fmt.Errorf("XLSX has no sheets")
	}

	var b strings.Builder
	charCount := 0

	for _, sheet := range sheets {
		header := fmt.Sprintf("--- Sheet: %s ---\n", sheet)
		if charCount+len(header) > maxChars {
			b.WriteString("\n[truncated]")
			break
		}
		b.WriteString(header)
		charCount += len(header)

		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}

		for _, row := range rows {
			line := strings.Join(row, "\t") + "\n"
			if charCount+len(line) > maxChars {
				remaining := maxChars - charCount
				if remaining > 0 {
					b.WriteString(line[:remaining])
				}
				b.WriteString("\n[truncated]")
				return strings.TrimSpace(b.String()), nil
			}
			b.WriteString(line)
			charCount += len(line)
		}
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return "", fmt.Errorf("no data could be extracted from this XLSX")
	}

	return result, nil
}
