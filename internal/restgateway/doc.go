// Package restgateway serves the REST surface and Swagger UI for the same
// Connect handlers that answer gRPC/Connect/gRPC-Web. vanguard transcodes
// google.api.http annotations at runtime; there is no generated gateway code.
//
// Construct with New(address, *connect.Server).
package restgateway
