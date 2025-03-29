# TinyTwamp

## An attempt at a TWAMP client/server in go

### Features
Supports Client and Server in the same binary. 

Supports IPv6 as well as legacy IP

### Example Usage:

To run the server on an IPv6-enabled machine:

`go run main.go -mode server`

Run as a server:

`go run tinytwamp.go -mode server [::1] -daemon`

To run the client targeting an IPv6 server:

`go run main.go -mode client -server [::1]`


