# wan-prober

This tool can periodically check the health of an internet connection
by probing targets on the internet and monitoring for responses.
It provides an HTTP API for checking the current health of the configured internet connections.

## Downloading

Download prebuilt binaries from [GitHub](https://github.com/adaricorp/wan-prober/releases/latest).

## Configuring

A configuration file must be provided which configures which interfaces to probe and which targets to probe.
An [example config](https://github.com/adaricorp/wan-prober/blob/main/sample-configs/wan-prober.yml)
is provided to show the format.

## Running

To run wan-prober with a configuration file at `/etc/wan-prober.yml` that has an HTTP API server
listening on `localhost:8020`:

```
wan_prober \
    --config-file /etc/wan-prober.yml \
    --http-listen-address localhost:8020
```

It is also possible to configure wan-prober by using environment variables:

```
WAN_PROBER_CONFIG_FILE="/etc/wan-prober.yml" wan_prober
```
