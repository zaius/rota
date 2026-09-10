package models

import (
	"reflect"
	"testing"
)

func TestNormalizeGeoFilters(t *testing.T) {
	t.Run("nil stays nil so update can mean not provided", func(t *testing.T) {
		got, err := NormalizeGeoFilters(nil)
		if err != nil || got != nil {
			t.Fatalf("got %v, %v; want nil, nil", got, err)
		}
	})

	t.Run("empty stays empty and non-nil so update can clear", func(t *testing.T) {
		got, err := NormalizeGeoFilters([]GeoFilter{})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("got %#v, %v; want empty non-nil slice", got, err)
		}
	})

	t.Run("trims, upper-cases, and dedupes", func(t *testing.T) {
		got, err := NormalizeGeoFilters([]GeoFilter{
			{CountryCode: " us "},
			{CountryCode: "US"},
			{CountryCode: "de", CityName: " Berlin "},
			{CountryCode: "DE", CityName: "Berlin"},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []GeoFilter{{CountryCode: "US"}, {CountryCode: "DE", CityName: "Berlin"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("wildcard is accepted on its own", func(t *testing.T) {
		got, err := NormalizeGeoFilters([]GeoFilter{{CountryCode: GeoFilterAllCountries}})
		if err != nil || len(got) != 1 || !got[0].IsAllCountries() {
			t.Fatalf("got %#v, %v", got, err)
		}
		if !HasAllCountries(got) {
			t.Fatal("HasAllCountries should be true")
		}
	})

	t.Run("wildcard with a city is rejected", func(t *testing.T) {
		if _, err := NormalizeGeoFilters([]GeoFilter{{CountryCode: "*", CityName: "Paris"}}); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty or malformed codes are rejected", func(t *testing.T) {
		for _, bad := range []string{"", "   ", "U", "USAX"} {
			if _, err := NormalizeGeoFilters([]GeoFilter{{CountryCode: bad}}); err == nil {
				t.Errorf("country_code %q: expected error", bad)
			}
		}
	})
}
