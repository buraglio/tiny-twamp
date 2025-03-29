
# TWAMP Server and Client in Go

This is a simple implementation of a Two-Way Active Measurement Protocol (TWAMP) server and client in Go. The server can run interactively or as a daemon, and the client can perform round-trip time (RTT) tests between itself and the server. Logs of each test are captured on both the client and the server side.

## Features

- **TWAMP Server**: Listens for test packets from a client and responds with an echo.
- **TWAMP Client**: Sends test packets to the server, calculates the round-trip time (RTT), and sends the RTT result back to the server.
- **Logging**: Detailed logs of each test are captured, including:
  - Client request logs (including the test message sent).
  - Server response logs (including the response sent).
  - Test result logs (including round-trip time).

## Requirements

- Go 1.16 or higher
- An environment with IPv6 support

## Installation

1. Clone the repository to your local machine:
    ```bash
    git clone https://github.com/yourusername/twamp-go.git
    cd twamp-go
    ```

2. Build the project:
    ```bash
    go build
    ```

3. Run the server or client (see below for usage).

## Usage

### Server Mode

You can run the server either interactively or as a daemon.

- **Interactive Mode**:
    To run the server in interactive mode and log results to a specified file:
    ```bash
    go run tinytwamp.go -mode server -logfile /path/to/logfile.log
    ```

- **Daemon Mode**:
    To run the server as a daemon (background process) and log results:
    ```bash
    go run tinytwamp.go -mode server -daemon true -logfile /path/to/logfile.log
    ```

### Client Mode

To run the client and perform a test against the server, use the following command:
```bash
go run tinytwamp.go -mode client -server fd7a:115c:a1e0::1801:7746 -logfile /path/to/logfile.log
```

- Replace `fd7a:115c:a1e0::1801:7746` with the server's IPv6 address.
- The `-logfile` flag is optional, and if omitted, logs will be printed to `stdout`.

### Command-Line Flags

- `-mode`: Specifies whether the program should run as a "client" or "server" (default is "client").
- `-server`: Specifies the server's IPv6 address (used only in client mode).
- `-daemon`: If true, runs the server as a daemon (background process).
- `-logfile`: Path to a file where logs will be saved. If not provided, logs will be printed to `stdout`.

## Logs

- The server logs all received test packets, sent responses, and the results of each test (including round-trip time).
- The client logs the round-trip time for each test.
- Example log entries:

### Server Logs:
```
2025/03/29 14:09:01 Received test packet from [fd7a:115c:a1e0::1801:7746]: "TWAMP test message"
2025/03/29 14:09:01 Sent response to [fd7a:115c:a1e0::1801:7746]
2025/03/29 14:09:01 Test result for client [fd7a:115c:a1e0::1801:7746]: Round-trip time received: Round-trip time: 20ms
```

### Client Logs:
```
2025/03/29 14:09:01 Round-trip time: 20ms
2025/03/29 14:09:01 Client logged round-trip time: 20ms
```

## Contribution

Feel free to fork this project and submit pull requests. If you have any issues or feature requests, please open an issue in the GitHub repository.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
