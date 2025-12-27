package dupes

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

// FormatSize formats bytes to human readable string
func FormatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %c", float64(b)/float64(div), "KMGTPE"[exp])
}
