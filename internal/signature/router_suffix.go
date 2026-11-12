package signature

// maxRouterSuffixLen bounds the dot-delimited suffix a router such as
// OpenRouter appends after a native signature envelope. Observed values are
// well under this bound.
const maxRouterSuffixLen = 128

// routerSuffixCharSet is the alphanumeric-only alphabet observed on
// router-appended suffixes; base64url's '-'/'_' and any second '.' are all
// rejected so the suffix can never be mistaken for more envelope payload.
var routerSuffixCharSet = base64AlphabetSet("")

func isValidRouterSuffix(suffix string) bool {
	if len(suffix) == 0 || len(suffix) > maxRouterSuffixLen {
		return false
	}
	for index := 0; index < len(suffix); index++ {
		if !routerSuffixCharSet[suffix[index]] {
			return false
		}
	}
	return true
}
