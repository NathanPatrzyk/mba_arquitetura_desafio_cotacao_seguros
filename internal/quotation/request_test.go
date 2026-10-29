package quotation

import "testing"

func TestNormalizeFillsDefaultsAndTidiesUp(t *testing.T) {
	request := Request{
		Driver:  Driver{Document: "  12345678901 ", BirthYear: 1988},
		Vehicle: Vehicle{Plate: " abc1d23 ", Model: " Gol 1.0 ", Year: 2020, ValueCents: 8500000},
	}

	if err := request.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if request.Coverage != defaultCoverage {
		t.Errorf("coverage %q, expected %q", request.Coverage, defaultCoverage)
	}
	if request.Vehicle.Plate != "ABC1D23" {
		t.Errorf("plate %q, expected ABC1D23", request.Vehicle.Plate)
	}
	if request.Driver.Document != "12345678901" {
		t.Errorf("document %q, expected no surrounding spaces", request.Driver.Document)
	}
}

func TestNormalizeRejectsIncompleteRequest(t *testing.T) {
	complete := func() Request {
		return Request{
			Driver:  Driver{Document: "12345678901", BirthYear: 1988},
			Vehicle: Vehicle{Plate: "ABC1D23", Year: 2020, ValueCents: 8500000},
		}
	}

	cases := map[string]func(*Request){
		"missing document":     func(r *Request) { r.Driver.Document = "" },
		"missing birth year":   func(r *Request) { r.Driver.BirthYear = 0 },
		"missing plate":        func(r *Request) { r.Vehicle.Plate = "" },
		"missing vehicle year": func(r *Request) { r.Vehicle.Year = 0 },
		"zero value":           func(r *Request) { r.Vehicle.ValueCents = 0 },
		"negative value":       func(r *Request) { r.Vehicle.ValueCents = -1 },
		"invalid coverage":     func(r *Request) { r.Coverage = "vip" },
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			request := complete()
			breakIt(&request)
			if err := request.Normalize(); err == nil {
				t.Fatalf("invalid request was accepted: %+v", request)
			}
		})
	}
}