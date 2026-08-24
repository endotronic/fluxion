package util

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ParseSize parses strings like "10M", "1.5G", "1024" into bytes
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}

	// Find split between number and unit
	lastDigit := -1
	for i, r := range s {
		if unicode.IsDigit(r) || r == '.' {
			lastDigit = i
		} else {
			break
		}
	}
	
	if lastDigit == -1 {
		return 0, fmt.Errorf("invalid size format")
	}
	
	valStr := s[:lastDigit+1]
	unitStr := s[lastDigit+1:]
	
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %v", err)
	}
	
	unitStr = strings.TrimSpace(unitStr)
	var multiplier float64 = 1
	
	switch unitStr {
	case "", "B":
		multiplier = 1
	case "K", "KB":
		multiplier = 1024
	case "M", "MB":
		multiplier = 1024 * 1024
	case "G", "GB":
		multiplier = 1024 * 1024 * 1024
	case "T", "TB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown unit: %s", unitStr)
	}
	
	return int64(val * multiplier), nil
}

// FormatBytes formats bytes to human readable string using IEC (1024) units
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Comma renders a count with thousands separators. File counts in this tool run
// to eight digits, and an unseparated one is unreadable.
func Comma(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
