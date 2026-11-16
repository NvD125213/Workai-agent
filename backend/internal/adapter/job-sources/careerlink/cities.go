package careerlink

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// City is one province of CareerLink's location filter, the last two segments of a
// search URL: /tim-viec-lam-tai/k/{keyword}/{slug}/{code} (ha-noi/HN).
type City struct {
	Code string `json:"code"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// cities.json holds the 63 provinces (the codes the site still uses after the 2025
// merger). The site publishes no list: each code was confirmed by requesting
// /tim-viec-lam-tai/k/x/zz/{code}, which CareerLink redirects to the province's own slug.
//
//go:embed cities.json
var citiesJSON []byte

var cityIndex = sync.OnceValue(func() map[string]City {
	var list []City
	if err := json.Unmarshal(citiesJSON, &list); err != nil {
		panic("careerlink: bad cities.json: " + err.Error())
	}
	byKey := make(map[string]City, 2*len(list))
	for _, c := range list {
		byKey[c.Slug] = c
		byKey[htmltext.Slugify(c.Name)] = c
	}
	return byKey
})

// cityAliases maps other ways people write a province to its slug in cities.json.
var cityAliases = map[string]string{
	"hn":             "ha-noi",
	"hcm":            "ho-chi-minh",
	"tp-hcm":         "ho-chi-minh",
	"tphcm":          "ho-chi-minh",
	"tp-ho-chi-minh": "ho-chi-minh",
	"sai-gon":        "ho-chi-minh",
	"saigon":         "ho-chi-minh",
	"thua-thien-hue": "hue",
	"vung-tau":       "ba-ria-vung-tau",
	"bac-can":        "bac-kan",
	"dn":             "da-nang",
}

// LookupCity finds a province by name, slug or code, ignoring case and accents:
// "Hà Nội", "ha-noi", "HN" give HN; "TP.HCM", "Sài Gòn" give HCM.
func LookupCity(nameOrSlug string) (City, bool) {
	key := htmltext.Slugify(nameOrSlug)
	for _, prefix := range []string{"thanh-pho-", "tp-", "tinh-"} {
		key = strings.TrimPrefix(key, prefix)
	}
	if alias, ok := cityAliases[key]; ok {
		key = alias
	}
	if c, ok := cityIndex()[key]; ok {
		return c, true
	}
	code := strings.ToUpper(strings.TrimSpace(nameOrSlug))
	for _, c := range cityIndex() {
		if c.Code == code {
			return c, true
		}
	}
	return City{}, false
}
