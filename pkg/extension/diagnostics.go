package extension

import "errors"

const MethodDiagnosticEmit = "diagnostics.emit"

type Diagnostic struct {
	Level     string `json:"level"`
	Code      string `json:"code"`
	Phase     string `json:"phase,omitempty"`
	Status    int    `json:"status,omitempty"`
	Millis    int64  `json:"millis,omitempty"`
	Count     int64  `json:"count,omitempty"`
	Retryable *bool  `json:"retryable,omitempty"`
}

type DiagnosticResult struct {
	Recorded bool `json:"recorded"`
}

func ValidateDiagnostic(manifest Manifest, value Diagnostic) error {
	if manifest.Contract.Version.Minor < 7 {
		return errors.New("structured diagnostics require SDK API 1.7")
	}
	if value.Level != "debug" && value.Level != "info" && value.Level != "warn" && value.Level != "error" {
		return errors.New("invalid diagnostic level")
	}
	if !diagnosticCode(value.Code) || value.Phase != "" && !diagnosticCode(value.Phase) ||
		value.Status != 0 && (value.Status < 100 || value.Status > 599) ||
		value.Millis < 0 || value.Millis > 24*60*60*1000 || value.Count < 0 || value.Count > 1<<53-1 {
		return errors.New("invalid bounded diagnostic facts")
	}
	return nil
}

func diagnosticCode(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '.' && character != '_' && character != '-' {
					return false
				}
			}
		}
	}
	return true
}
