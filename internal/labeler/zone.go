package labeler

import (
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ZoneLabel is the well-known topology label the labeler manages.
const ZoneLabel = "topology.kubernetes.io/zone"

// ZoneFromRack turns a NetBox rack name into the label value: lower-cased
// (cloud providers publish lower-case zones) with runs of whitespace
// collapsed into a single dash.
func ZoneFromRack(rack string) string {
	return strings.ToLower(strings.Join(strings.Fields(rack), "-"))
}

// ValidateZone reports why zone cannot be used as a label value.
func ValidateZone(zone string) error {
	if zone == "" {
		return errors.New("empty value")
	}
	if errs := validation.IsValidLabelValue(zone); len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
