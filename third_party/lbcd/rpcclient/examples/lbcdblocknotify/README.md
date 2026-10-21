# lbcdbloknotify

This bridge program subscribes to lbcd's notifications over websockets using the rpcclient package.
Users can specify supported actions upon receiving this notifications.

## Building(or Running) the Program

Clone the lbcd package:

```bash
$ git clone github.com/lbryio/lbcd
$ cd lbcd/rpcclient/examples

# build the program
$ go build .

# or directly run it (build implicitly behind the scene)
$ go run .
```

Display available options:

```bash
$ go run . -h

  -coinid string
        Coin ID (default "1425")
  -rpcpass string
        LBCD RPC password (default "rpcpass")
  -rpcserver string
        LBCD RPC server (default "localhost:9245")
  -rpcuser string
        LBCD RPC username (default "rpcuser")
  -stratum string
        Stratum server (default "lbrypool.net:3334")
  -stratumpass string
        Stratum server password (default "password")
  -quiet
        Do not print periodic logs
```

Running the program:

```bash
# Send stratum mining.update_block mesage upon receving block connected notifiations.
$ go run . -rpcuser <RPC USERNAME> -rpcpass <RPC PASSWD> --notls -stratum <STRATUM SERVER> -stratumpass <STRATUM PASSWD>

2022/01/10 23:16:21 Current block count: 1093112
...

# Execute a custome command (with blockhash) upon receving block connected notifiations.
$ go run . -rpcuser <RPC USERNAME> -rpcpass <RPC PASSWD> --notls -run "echo %s"
```

## Notes

* Stratum TCP connection is persisted with auto-reconnect. (retry backoff increases from 1s to 60s maximum)

* Stratum update_block jobs on previous notifications are canceled when a new notification arrives.
  Usually, the jobs are so short and completed immediately.  However, if the Stratum connection is broken, this
  prevents the bridge from accumulating stale jobs.

## License

This example is licensed under the [copyfree](http://copyfree.org) ISC License.
