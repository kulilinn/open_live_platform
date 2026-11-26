package admin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGiftWorkbookFormats(t *testing.T) {
	// Each Excel-authored fixture has a 64-bit text ID, 1904 dates, and a formula
	// whose saved value is 42 even though its input cell now contains 99.
	for _, extension := range []string{"xlsx", "xls", "ods", "xlsb"} {
		t.Run(extension, func(t *testing.T) {
			var python string
			if extension == "xlsb" {
				python = os.Getenv("OPEN_LIVE_TEST_XLSB_PYTHON")
				if python == "" {
					t.Skip("set OPEN_LIVE_TEST_XLSB_PYTHON to test the optional XLSB reader")
				}
			}
			input, err := os.Open(filepath.Join("testdata", "gift_workbooks", "product_limit_config."+extension))
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			batch, err := readGiftSheet(t.Context(), "product_limit_config", input, python)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Limits) != 1 {
				t.Fatalf("expected one limit, got %d", len(batch.Limits))
			}
			item := batch.Limits[0]
			start := time.Date(2031, time.February, 3, 4, 5, 6, 0, time.UTC)
			end := time.Date(2031, time.February, 4, 4, 5, 6, 0, time.UTC)
			if item.ID != 9007199254740993 || item.GiftID != 25 || item.PromotionID != 41 || item.Price != 42 ||
				item.Income == nil || *item.Income != 12 || !item.Starts.Equal(start) || !item.Ends.Equal(end) {
				t.Fatalf("imported values changed: id=%d gift=%d promotion=%d income=%v price=%d starts=%s ends=%s",
					item.ID, item.GiftID, item.PromotionID, item.Income, item.Price, item.Starts, item.Ends)
			}
		})
	}
}

func TestGiftWorkbookXLSBNeedsReader(t *testing.T) {
	input, err := os.Open(filepath.Join("testdata", "gift_workbooks", "product_limit_config.xlsb"))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	_, err = readGiftSheet(t.Context(), "product_limit_config", input, "")
	if !errors.Is(err, errXLSBReaderUnavailable) {
		t.Fatalf("unexpected result without optional reader: %v", err)
	}
}
