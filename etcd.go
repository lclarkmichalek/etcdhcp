package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"strings"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

var (
	etcdDiscoverySRV       = flag.String("etcd.discovery.srv", "", "SRV record to use to discover ETCD")
	etcdDiscoveryEndpoints = flag.String("etcd.discovery.endpoints", "", "endpoint list to use to discover ETCD")
	etcdAuthUsername       = flag.String("etcd.auth.username", "", "username used to auth with ETCD")
	etcdAuthPassword       = flag.String("etcd.auth.password", "", "password used to auth with ETCD")
	etcdAuthCA             = flag.String("etcd.auth.ca", "", "CA certificate used to connect to ETCD")
	etcdAuthCert           = flag.String("etcd.auth.cert", "", "client certificate used to connect to ETCD")
	etcdAuthKey            = flag.String("etcd.auth.key", "", "client key used to connect to ETCD")
	etcdPrefix             = flag.String("etcd.prefix", "etcdhcp::", "prefix to use when calculating ETCD keys")
)

func NewETCDStore(ctx context.Context) (*etcd.Client, error) {
	conf, err := etcdConfig()
	if err != nil {
		return nil, errors.WithMessage(err, "could not load etcd config")
	}

	client, err := etcd.New(conf)
	if err != nil {
		return nil, errors.Wrap(err, "could not create etcd client")
	}

	err = client.Sync(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "could not perform initial etcd endpoint sync")
	}

	go func() {
		for ctx.Err() == nil {
			func() {
				ctx, cancel := context.WithTimeout(ctx, time.Second*30)
				defer cancel()

				err := client.Sync(ctx)
				if err != nil {
					glog.Errorf("failed to sync etcd endpoints: %v", err)
				} else {
					glog.V(4).Infof("synced etcd endpoint list")
				}
			}()

			select {
			case <-time.After(time.Second * 60):
			case <-ctx.Done():
			}
		}
	}()

	return client, nil
}

func etcdConfig() (etcd.Config, error) {
	https := false
	certificates := []tls.Certificate{}
	if *etcdAuthCert != "" {
		if *etcdAuthKey == "" {
			return etcd.Config{}, errors.New("-etcd.auth.cert requires -etcd.auth.key")
		}
		https = true

		cert, err := tls.LoadX509KeyPair(*etcdAuthCert, *etcdAuthKey)
		if err != nil {
			return etcd.Config{}, errors.Wrap(err, "could not load etcd client key pair")
		}

		certificates = append(certificates, cert)
	}

	caCertPool := x509.NewCertPool()
	if *etcdAuthCA != "" {
		https = true
		caCert, err := ioutil.ReadFile(*etcdAuthCA)
		if err != nil {
			return etcd.Config{}, errors.Wrap(err, "could not load etcd CA")
		}

		caCertPool.AppendCertsFromPEM(caCert)
	}

	endpoints := []string{}
	if *etcdDiscoverySRV != "" {
		_, addrs, err := net.LookupSRV("", "", *etcdDiscoverySRV)
		if err != nil {
			return etcd.Config{}, errors.Wrapf(err, "could not resolve discovery SRV %v", etcdDiscoverySRV)
		}
		for _, addr := range addrs {
			endpoints = append(endpoints, fmt.Sprintf("%v:%v", addr.Target, addr.Port))
		}
	} else if *etcdDiscoveryEndpoints != "" {
		endpoints = strings.Split(*etcdDiscoveryEndpoints, ",")
	} else {
		return etcd.Config{}, errors.New("No etcd discovery mechanism specified (-etcd.discovery.srv, -etcd.discovery.endpoints)")
	}

	for _, e := range endpoints {
		u, err := url.Parse(e)
		if err != nil {
			return etcd.Config{}, errors.Wrapf(err, "could not parse initial node %v", e)
		}
		if https && u.Scheme != "https" {
			return etcd.Config{}, errors.Errorf("HTTPS configured, but non https address found in initial nodes: %v", e)
		}
	}

	var tlsConfig *tls.Config
	if https {
		tlsConfig = &tls.Config{
			Certificates: certificates,
			RootCAs:      caCertPool,
		}
	}

	return etcd.Config{
		Endpoints: endpoints,
		TLS:       tlsConfig,
		Username:  *etcdAuthUsername,
		Password:  *etcdAuthPassword,
	}, nil
}
