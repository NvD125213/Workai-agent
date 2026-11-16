package onetwothreejob

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// City is one province of 123job's location filter (?cityId=1 is Ha Noi).
type City struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// cities.json is the "provinces" table of the site's own location module
// (/vendor/app/build/assets/locationData-*.js): 63 provinces, IDs 1-63.
//
//go:embed cities.json
var citiesJSON []byte

var cityIndex = sync.OnceValue(func() map[string]City {
	var list []City
	if err := json.Unmarshal(citiesJSON, &list); err != nil {
		panic("123job: bad cities.json: " + err.Error())
	}
	byKey := make(map[string]City, len(list))
	for _, c := range list {
		byKey[htmltext.Slugify(c.Name)] = c
	}
	return byKey
})

// cityAliases maps other ways people write a province to the slug of its name in cities.json.
var cityAliases = map[string]string{
	"hn":             "ha-noi",
	"tp-ha-noi":      "ha-noi",
	"hcm":            "ho-chi-minh",
	"tp-hcm":         "ho-chi-minh",
	"tphcm":          "ho-chi-minh",
	"tp-ho-chi-minh": "ho-chi-minh",
	"sai-gon":        "ho-chi-minh",
	"saigon":         "ho-chi-minh",
	"hue":            "thua-thien-hue",
	"vung-tau":       "ba-ria-vung-tau",
	"bac-kan":        "bac-can", // the site spells it "Bắc Cạn"
	"dn":             "da-nang",
	"tp-da-nang":     "da-nang",
}

// LookupCity finds a province by name or slug, ignoring case and accents: "Hà Nội",
// "ha-noi", "HN" give ID 1; "TP.HCM", "Sài Gòn" give 63.
func LookupCity(nameOrSlug string) (City, bool) {
	key := htmltext.Slugify(nameOrSlug)
	key = strings.TrimPrefix(strings.TrimPrefix(key, "thanh-pho-"), "tinh-")
	if alias, ok := cityAliases[key]; ok {
		key = alias
	}
	c, ok := cityIndex()[key]
	return c, ok
}
