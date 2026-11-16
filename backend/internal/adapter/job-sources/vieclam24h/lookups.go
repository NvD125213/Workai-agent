package vieclam24h

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// lookups.json holds the code tables the site itself uses, taken from its page data
// (__NEXT_DATA__: initialState.AuLocation.provinces and api.initCommon.data):
//   - provinces: both versions. Version 2 (210000xxx) is the 34 provinces after the
//     2025 merger, what job items carry in province_ids; version 1 (110000xxx, with
//     old_id 73 = Ha Noi...) is the pre-merger list. The search API accepts either id.
//   - occupations: ID -> slug, the first part of a job URL ("it-phan-mem").
//   - experience_range, level_requirement, degree_requirement, working_method, gender:
//     the coded fields of a job item.
//
//go:embed lookups.json
var lookupsJSON []byte

type Province struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Version int    `json:"version"`
	OldID   int    `json:"old_id,omitempty"` // version 1 only: the id used before (73 = Ha Noi)
}

type occupation struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type lookupTables struct {
	Provinces         []Province        `json:"provinces"`
	Occupations       []occupation      `json:"occupations"`
	ExperienceRange   map[string]string `json:"experience_range"`
	LevelRequirement  map[string]string `json:"level_requirement"`
	DegreeRequirement map[string]string `json:"degree_requirement"`
	WorkingMethod     map[string]string `json:"working_method"`
	Gender            map[string]string `json:"gender"`

	provinceByID   map[int]Province
	provinceByKey  map[string]Province // slug of name or slug -> province, version 2 preferred
	occupationByID map[int]occupation
}

var lookups = sync.OnceValue(func() *lookupTables {
	t := &lookupTables{}
	if err := json.Unmarshal(lookupsJSON, t); err != nil {
		panic("vieclam24h: bad lookups.json: " + err.Error())
	}
	t.provinceByID = make(map[int]Province, len(t.Provinces))
	t.provinceByKey = make(map[string]Province, len(t.Provinces)*2)
	for _, p := range t.Provinces {
		t.provinceByID[p.ID] = p
		for _, k := range []string{p.Slug, htmltext.Slugify(p.Name)} {
			// Version 2 wins; a pre-merger province name ("Bắc Giang") keeps its v1 entry.
			if cur, ok := t.provinceByKey[k]; !ok || cur.Version != 2 {
				t.provinceByKey[k] = p
			}
		}
	}
	t.occupationByID = make(map[int]occupation, len(t.Occupations))
	for _, o := range t.Occupations {
		t.occupationByID[o.ID] = o
	}
	return t
})

// provinceAliases maps other ways people write a province to a slug of lookups.json.
var provinceAliases = map[string]string{
	"hn":             "ha-noi",
	"tp-ha-noi":      "ha-noi",
	"hcm":            "ho-chi-minh",
	"tp-hcm":         "ho-chi-minh",
	"tphcm":          "ho-chi-minh",
	"tp-ho-chi-minh": "ho-chi-minh",
	"sai-gon":        "ho-chi-minh",
	"saigon":         "ho-chi-minh",
	"thua-thien-hue": "hue",
	"dn":             "da-nang",
	"tp-da-nang":     "da-nang",
}

// LookupProvince finds a province by name or slug, ignoring case and accents ("Hà Nội",
// "ha noi", "HN", "TP.HCM"...). Current (post-2025) provinces come first; a province that
// was merged ("Bắc Giang") gives its pre-merger entry, which the search API accepts too.
func LookupProvince(nameOrSlug string) (Province, bool) {
	key := htmltext.Slugify(nameOrSlug)
	key = strings.TrimPrefix(strings.TrimPrefix(key, "thanh-pho-"), "tinh-")
	if alias, ok := provinceAliases[key]; ok {
		key = alias
	}
	p, ok := lookups().provinceByKey[key]
	return p, ok
}

// ProvinceByID finds a province by any of its IDs (version 2, version 1 or the old id).
func ProvinceByID(id int) (Province, bool) {
	t := lookups()
	if p, ok := t.provinceByID[id]; ok {
		return p, true
	}
	for _, p := range t.Provinces {
		if p.OldID == id {
			return p, true
		}
	}
	return Province{}, false
}

func occupationSlug(id int) string {
	return lookups().occupationByID[id].Slug
}

func occupationName(id int) string {
	return lookups().occupationByID[id].Name
}

func codeName(table map[string]string, code int) string {
	if code <= 0 {
		return ""
	}
	return table[strconv.Itoa(code)]
}
