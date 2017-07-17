# etcDHCP

Ever wondered what was going on with your DHCP leases? Not comfortable looking
at `/var/lib/dhcp/dhcp.leases`? Feel like your DHCP should be a bit higher
availablilty, and a little bit more stateless? Prefer GRPC to ARP? Want to make
your DHCP server a legitimate microservice? Is dhcpd not cloud native enough for
you?

If any of that applies to you, I may have the DHCP server of your dreams, right
here. etcDHCP is the worlds first strongly consistent distributed DHCP server
(in the absence of any other google results), using CoreOS's etcd to ensure that
whatever happens, your leases will remain available. Well, at least, in 49% of
cases. Assuming all cases have an independent effect on your etcd nodes.

## Install

If you've never built a Go project before, I'm sorry.

```
$ go get -u github.com/golang/dep/cmd/dep
$ go get github.com/lclarkmichalek/etcdhcp
$ cd $GOPATH/src/github.com/lclarkmichalek/etcdhcp
$ dep ensure
$ go install
```

## Setup

### etcd

etcDHCP requires etcd, surprisingly enough. Notably, it requires at least
version 3.3.0. However, this version doesn't actually exist yet, so just run
master.

### etcDHCP

Because running `--help` is hard, I guess.

```
$ etcdhcp \
  -etcd.discovery.endpoints localhost:2379 \
  -v=2 \
  -dhcp.router 10.6.9.1 \
  -dhcp.dns 10.6.9.1 \
  -dhcp.server-if wlan0_ap \
  -dhcp.server-ip 10.6.9.1 \
  -dhcp.subnet-mask 255.255.255.0 \
  -dhcp.issue-from 10.6.9.10 \
  -dhcp.issue-to 10.6.9.100 \
I0717 17:39:22.076564   25273 main.go:51] starting dhcp listener
I0717 17:39:22.076564   25273 main.go:45] starting admin server on :9842
```

There are tons of etcd options, most of which will probably be useless, as if
you're not yet running a DHCP server, there's a decent chance that SRV record
resolution won't be working. Notably missing is discovery via MAC address. Now
that'd be fun..

The DHCP options should be, um, well, let's hope you've configured a DHCP server
before. Plenty of things aren't supported, but you can certainly get a lot of
stuff working by writing to etcd directly.

## Operating concerns

Like all ~~good~~ cloud native software, etcDHCP exports Prometheus metrics on
`:9842`. There's also pprof endpoints exposed. I didn't implement a readiness or
liveness endpoint, as um, well they'd be a bit pointless. Also no gracefull
shutdowns because github.com/krolaw/dhcp4 doesn't support context yet.

## etcd structure

```
$ ./etcdctl get etcdhcp::ips::leased::10.6.9.99
etcdhcp::ips::leased::10.6.9.99
5c:96:56:a4:be:eb
$ ./etcdctl get etcdhcp::ips::free::10.6.9.94
etcdhcp::ips::free::10.6.9.94
10.6.9.94
$ ./etcdctl get etcdhcp::nics::leased::08:9e:08:b5:af:01
etcdhcp::nics::leased::08:9e:08:b5:af:01
10.6.9.97
```

Want to add more IPs? Either restart the dhcp server with a different range,
or..

```
$ for i in `seq 200 220`; do ./etcdctl put etcdhcp::ips::free::10.6.9.$i 10.6.9.$i; done
```

Warning: IPs that aren't in the range of a running server will gradually be
phased out. They'll be removed when they are picked up by a client, and that
client disconnects.
