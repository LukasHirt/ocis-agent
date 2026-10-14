package main

import (
	"encoding/xml"
	"errors"
	"fmt"
)

type multistatus struct {
	Responses []struct {
		Propstats []struct {
			Prop struct {
				ETag string `xml:"getetag"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// parseETag returns the first non-empty getetag from a PROPFIND response,
// with its surrounding quotes, ready to use in If-Match.
func parseETag(body []byte) (string, error) {
	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return "", fmt.Errorf("parse PROPFIND response: %w", err)
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstats {
			if ps.Prop.ETag != "" {
				return ps.Prop.ETag, nil
			}
		}
	}
	return "", errors.New("no getetag in PROPFIND response")
}
