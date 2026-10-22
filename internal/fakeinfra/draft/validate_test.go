package draft

import (
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestInvalidCarriesFieldViolations(t *testing.T) {
	fields := map[string]string{
		"name":                           "Use lowercase letters, numbers, and hyphens.",
		"identityProvider.config.issuer": "Enter a valid HTTP or HTTPS URL.",
	}
	st := status.Convert(invalid(joinMessages(fields), fields))
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code %v", st.Code())
	}
	if want := "Enter a valid HTTP or HTTPS URL. Use lowercase letters, numbers, and hyphens."; st.Message() != want {
		t.Fatalf("summary %q, want %q", st.Message(), want)
	}
	var got []*errdetails.BadRequest_FieldViolation
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			got = append(got, br.GetFieldViolations()...)
		}
	}
	if len(got) != 2 || got[0].GetField() != "identityProvider.config.issuer" || got[1].GetField() != "name" {
		t.Fatalf("violations %v, want both fields in path order", got)
	}
	for _, v := range got {
		if v.GetDescription() != fields[v.GetField()] {
			t.Errorf("%s: %q", v.GetField(), v.GetDescription())
		}
	}
}
