package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/contracts"
)

func TestBuildPerFileContractExtractors_IncludesHtmx(t *testing.T) {
	idx := &Indexer{}
	_, byLang := idx.buildPerFileContractExtractors()
	for _, lang := range []string{"html", "gotmpl", "templ"} {
		found := false
		for _, ex := range byLang[lang] {
			if _, ok := ex.(*contracts.HtmxExtractor); ok {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("byLang[%q] has no HtmxExtractor", lang)
		}
	}
}
