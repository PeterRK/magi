package planner

import (
	"strings"
	"testing"
)

func TestValidateOrders(t *testing.T) {
	shared := "Implement the assigned behavior across its affected entry points. Preserve the existing public interface and unrelated behavior. Inspect the current implementation, reuse relevant earlier findings, add focused coverage for normal and failure paths, and run the relevant tests. Deliver the source changes and report observed results, unresolved questions, and any remaining limitations. "
	var unicodeSource strings.Builder
	for char := rune(0x4e00); char < 0x4e28; char++ {
		unicodeSource.WriteRune(char)
	}
	limitOrders := []string{"甲", "乙", "丙", "丁", "戊", "己", "庚", "辛", "壬"}
	for _, tc := range []struct {
		name         string
		instructions []string
		feedback     string
	}{
		{"empty", nil, ""},
		{"single", []string{"Implement cleanup"}, ""},
		{"short distinct", []string{"甲乙丙", "甲乙丁"}, ""},
		{"at limit", limitOrders[:8], ""},
		{"above limit", limitOrders, "at most 8 orders"},
		{"exact", []string{"A separate order", "Implement response cleanup", "Implement response cleanup"}, "orders[1] and orders[2] are duplicates"},
		{"normalized", []string{" \r\n核对\r\n结果\r", "核对\n结果"}, "are duplicates"},
		{"short duplicate", []string{"abc", "abc"}, "are duplicates"},
		{"near English", []string{shared + "Validate cleanup.", shared + "Verify response closure and error propagation."}, ">= 0.80"},
		// One middle-character edit scores about 0.805 for n=4 and 0.756 for n=5.
		{"near Unicode", []string{unicodeSource.String(), strings.Replace(unicodeSource.String(), "且", "替", 1)}, "4-character Jaccard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOrders(tc.instructions)
			if tc.feedback == "" {
				if err != nil {
					t.Fatal("independent orders rejected", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.feedback) {
				t.Fatal("missing dispatch rejection", err)
			}
		})
	}
}
