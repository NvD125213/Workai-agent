package joboko

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// Location is one province of Joboko's location filter. The search URL takes the
// numeric ID (?l=2 is Ha Noi); the slug alone is ignored by the site.
type Location struct {
	ID   int    `json:"id"`
	Slug string `json:"slug"` // e.g. "ha-noi"; "" for "Tất cả địa điểm" (ID 0)
	Name string `json:"name"` // e.g. "Hà Nội"
}

// location.json is the list of the site's "Địa điểm" dropdown (<select id="txtTinhThanh">).
// Districts have IDs too (e.g. 391 = Quận Cầu Giấy) but are not listed.
//
//go:embed location.json
var locationJSON []byte

// LocationAll is the "Tất cả địa điểm" filter.
const LocationAll = 0

var locations = sync.OnceValue(func() map[string]Location {
	var list []Location
	if err := json.Unmarshal(locationJSON, &list); err != nil {
		panic("joboko: bad location.json: " + err.Error())
	}
	byKey := make(map[string]Location, len(list)*2)
	for _, l := range list {
		byKey[l.Slug] = l
		byKey[htmltext.Slugify(l.Name)] = l
	}
	return byKey
})

// LookupLocation finds a province by name or slug, ignoring case and accents:
// "Hà Nội", "ha noi", "ha-noi", "HA NOI" all give ID 2. Common short forms of the big
// cities ("HCM", "TP.HCM", "Sài Gòn", "Huế"...) work too.
func LookupLocation(nameOrSlug string) (Location, bool) {
	key := htmltext.Slugify(nameOrSlug)
	key = strings.TrimPrefix(strings.TrimPrefix(key, "thanh-pho-"), "tinh-")
	if alias, ok := locationAliases[key]; ok {
		key = alias
	}
	l, ok := locations()[key]
	return l, ok
}

// locationAliases maps other ways people write a province to its slug.
var locationAliases = map[string]string{
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
	"dn":             "da-nang",
	"tp-da-nang":     "da-nang",
}

// Locations returns every province of the filter (with "Tất cả địa điểm" first), in
// the site's order.
func Locations() []Location {
	var list []Location
	_ = json.Unmarshal(locationJSON, &list)
	return list
}
