package requestpolicy

import "strings"

type Observation struct {
	Status int
	Code   string
	Type   string
}

func ObserveResponse(status int, code, kind string) Observation {
	if status < 100 || status > 599 {
		return Observation{}
	}
	return Observation{Status: status, Code: observationIdentifier(code, 256), Type: observationIdentifier(kind, 128)}
}

func observationIdentifier(value string, limit int) string {
	if len(value) > limit {
		return ""
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_.:-", character)) {
			return ""
		}
	}
	return value
}
