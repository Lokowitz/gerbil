# Gerbil

Gerbil is a simple [WireGuard](https://www.wireguard.com/) interface management server written in Go. Gerbil makes it easy to create WireGuard interfaces as well as add and remove peers with an HTTP API.

### Installation and Documentation

Gerbil can be used standalone with your own API, a static JSON file, or with Pangolin and Newt as part of the larger system. See documentation below:

-   [Installation Instructions](https://docs.fossorial.io)
-   [Full Documentation](https://docs.fossorial.io)

## Preview

<img src="public/screenshots/preview.png" alt="Preview"/>

_Sample output of a Gerbil container connected to Pangolin and terminating various peers._

## Key Functions

### Setup WireGuard

A WireGuard interface will be created and configured on the local Linux machine or in the Docker container according to the values given in either a JSON config file or via the remote server. If the interface already exists, it will be reconfigured.

### Manage Peers

Gerbil will create the peers defined in the config on the WireGuard interface. The HTTP API can be used to remove, create, and update peers on the interface dynamically.

### Report Bandwidth

Bytes transmitted in and out of each peer are collected every 10 seconds, and incremental usage is reported via the "reportBandwidthTo" endpoint. This can be used to track data usage of each peer on the remote server.

## CLI Args

- `reachableAt`: How should the remote server reach Gerbil's API?
- `generateAndSaveKeyTo`: Where to save the generated WireGuard private key to persist across restarts.
- `remoteConfig` (optional): Remote config location to HTTP get the JSON based config from. See `example_config.json`
- `config` (optional): Local JSON file path to load config. Used if remote config is not supplied. See `example_config.json`

Note: You must use either `config` or `remoteConfig` to configure WireGuard.

- `reportBandwidthTo` (optional): Remote HTTP endpoint to send peer bandwidth data
- `interface` (optional): Name of the WireGuard interface created by Gerbil. Default: `wg0`
- `listen` (optional): Port to listen on for HTTP server. Default: `3003`
- `log-level` (optional): The log level to use. Default: INFO

Example:

```bash
./gerbil \
--reachableAt=http://gerbil:3003 \
--generateAndSaveKeyTo=/var/config/key \
--remoteConfig=http://pangolin:3001/api/v1/gerbil/get-config \
--reportBandwidthTo=http://pangolin:3001/api/v1/gerbil/receive-bandwidth
```

```yaml
services:
  gerbil:
    image: fosrl/gerbil
    container_name: gerbil
    restart: unless-stopped
    command:
      - --reachableAt=http://gerbil:3003
      - --generateAndSaveKeyTo=/var/config/key
      - --remoteConfig=http://pangolin:3001/api/v1/gerbil/get-config
      - --reportBandwidthTo=http://pangolin:3001/api/v1/gerbil/receive-bandwidth
    volumes:
      - ./config/:/var/config
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    ports:
      - 51820:51820/udp
```

## Build

### Container 

Ensure Docker is installed.

```bash
make
```

### Binary

Make sure to have Go 1.23.1 installed.

```bash
make local
```

## Licensing

Gerbil is dual licensed under the AGPLv3 and the Fossorial Commercial license. For inquiries about commercial licensing, please contact us.

## Contributions

Please see [CONTRIBUTIONS](./CONTRIBUTING.md) in the repository for guidelines and best practices.
