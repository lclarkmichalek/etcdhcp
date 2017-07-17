package main

import (
	"context"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/golang/glog"
	dhcp "github.com/krolaw/dhcp4"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	adminAddr      = flag.String("admin-addr", ":9842", "address to serve admin interface on")
	serverIP       = flag.String("dhcp.server-ip", "10.0.0.1", "dhcp server ip")
	serverIF       = flag.String("dhcp.server-if", "eth0", "interface to serve on")
	subnetMask     = flag.String("dhcp.subnet-mask", "255.255.240.0", "the subnet to serve")
	router         = flag.String("dhcp.router", "10.0.0.1", "gateway to point clients to")
	dns            = flag.String("dhcp.dns", "10.0.0.1", "dns server to point clients to")
	issueFrom      = flag.String("dhcp.issue-from", "10.0.0.10", "first ip address to issue to clients")
	issueTo        = flag.String("dhcp.issue-to", "10.0.0.100", "last ip address to issue to clients")
	leaseDuration  = flag.Duration("dhcp.lease", time.Hour, "dhcp lease duration")
	requestTimeout = flag.Duration("dhcp.request-timeout", 200*time.Millisecond*200, "dhcp request processing timeout")
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	flag.Set("logtostderr", "true")
	flag.Parse()

	etcd, err := NewETCDStore(ctx)
	if err != nil {
		glog.Exitf("could not connect to etcd: %v", err)
	}
	handler := &DHCPHandler{
		client:        etcd,
		prefix:        *etcdPrefix,
		timeout:       *requestTimeout,
		leaseDuration: *leaseDuration,
		start:         parseIP4(*issueFrom),
		end:           parseIP4(*issueTo),
		ip:            parseIP4(*serverIP),
		options: dhcp.Options{
			dhcp.OptionSubnetMask:       parseIP4(*subnetMask),
			dhcp.OptionRouter:           parseIP4(*router),
			dhcp.OptionDomainNameServer: parseIP4(*dns),
		},
	}

	err = handler.bootstrapLeasableRange(ctx)
	if err != nil {
		glog.Exitf("failed to initialise etcd with leasable range: %v", err)
	}

	grp, ctx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		http.Handle("/metrics", promhttp.Handler())
		glog.Infof("starting admin server on %v", *adminAddr)
		err := listenAndServe(ctx, *adminAddr, http.DefaultServeMux)
		return errors.Wrap(err, "could not serve admin server")
	})

	grp.Go(func() error {
		glog.Infof("starting dhcp listener")
		err := dhcp.ListenAndServeIf(*serverIF, handler)
		return errors.Wrap(err, "could not listen to dhcp")
	})

	grp.Go(func() error {
		glog.Infof("starting lease monitor")
		err := handler.monitorLeases(ctx)
		return errors.Wrap(err, "could not monitor leases")
	})

	err = grp.Wait()
	if err != nil && errors.Cause(err) != context.Canceled {
		glog.Exitf("%v", err.Error())
	}
}

func listenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:         addr,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      handler,
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(ctx)
	}()
	err := srv.ListenAndServe()
	if ctx.Err() == nil {
		return err
	}
	return nil
}
