package main

import (
	"context"
	"flag"
	"net"
	"strings"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	etcdutil "github.com/coreos/etcd/clientv3/clientv3util"
	etcdpb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/golang/glog"
	dhcp "github.com/krolaw/dhcp4"
	"github.com/pkg/errors"
)

var (
	monitorInterval = flag.Duration("dhcp.monitor-interval", time.Minute*5, "period to resurrect ips from expired leases at")
)

func (h *DHCPHandler) bootstrapLeasableRange(ctx context.Context) error {
	kvc := etcd.NewKV(h.client)
	for ip := h.start; !ip.Equal(h.end); ip = dhcp.IPAdd(ip, 1) {
		freeIPKey := h.prefix + "ips::free::" + ip.String()
		leasedIPKey := h.prefix + "ips::leased::" + ip.String()
		res, err := kvc.Txn(ctx).If(
			etcdutil.KeyMissing(freeIPKey),
			etcdutil.KeyMissing(leasedIPKey),
		).Then(
			etcd.OpPut(freeIPKey, ip.String()),
		).Commit()
		if err != nil {
			return errors.Wrap(err, "could not move ip to free state")
		}
		if res.Succeeded {
			glog.Infof("established %v as free", ip)
		}
	}
	return nil
}

func (h *DHCPHandler) monitorLeases(ctx context.Context) error {
	t := time.NewTicker(*monitorInterval)
	defer t.Stop()
	for {
		err := h.resurrectLeases(ctx)
		if err != nil {
			glog.Errorf("could not resurrect leases: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (h *DHCPHandler) resurrectLeases(ctx context.Context) error {
	kvc := etcd.NewKV(h.client)
	leasedIPPrefix := h.prefix + "ips::leased::"
	glog.V(2).Infof("listing ips under %v", leasedIPPrefix)
	resp, err := kvc.Get(ctx, leasedIPPrefix, etcd.WithPrefix())
	if err != nil {
		return errors.Wrap(err, "could not list leased ips")
	}

	leased := map[string]struct{}{}
	for _, kv := range resp.Kvs {
		parts := strings.Split(string(kv.Key), "::")
		ip := parts[len(parts)-1]

		leased[ip] = struct{}{}
	}

	freeIPPrefix := h.prefix + "ips::free::"
	glog.V(2).Infof("listing ips under %v", freeIPPrefix)
	resp, err = kvc.Get(ctx, freeIPPrefix, etcd.WithPrefix())
	if err != nil {
		return errors.Wrap(err, "could not list free ips")
	}

	free := map[string]struct{}{}
	for _, kv := range resp.Kvs {
		parts := strings.Split(string(kv.Key), "::")
		ip := parts[len(parts)-1]

		free[ip] = struct{}{}
	}

	for ip := h.start; !ip.Equal(h.end); ip = dhcp.IPAdd(ip, 1) {
		if _, ok := free[ip.String()]; ok {
			continue
		}
		if _, ok := leased[ip.String()]; ok {
			continue
		}

		glog.V(2).Infof("moving %v from leased to free", ip)
		freeIPKey := h.prefix + "ips::free::" + ip.String()
		leasedIPKey := h.prefix + "ips::leased::" + ip.String()

		res, err := kvc.Txn(ctx).If(
			etcdutil.KeyMissing(freeIPKey),
			etcdutil.KeyMissing(leasedIPKey),
		).Then(
			etcd.OpPut(freeIPKey, ip.String()),
		).Commit()
		if err != nil {
			return errors.Wrap(err, "could not move ip to free state")
		}
		if res.Succeeded {
			glog.Infof("resurrected %v", ip)
		}
	}
	return nil
}

func (h *DHCPHandler) nicLeasedIP(ctx context.Context, nic string) (net.IP, error) {
	kvc := etcd.NewKV(h.client)
	key := h.prefix + "nics::leased::" + nic
	glog.V(2).Infof("GET %v", key)
	resp, err := kvc.Get(ctx, key)
	if err != nil {
		return nil, errors.Wrap(err, "could not get etcd key")
	}
	if len(resp.Kvs) == 0 {
		return nil, nil
	}
	return parseIP4(string(resp.Kvs[0].Value)), nil
}

func (h *DHCPHandler) freeIP(ctx context.Context) (net.IP, error) {
	kvc := etcd.NewKV(h.client)
	prefix := h.prefix + "ips::free::"
	glog.V(2).Infof("GET PREFIX(%v)", prefix)
	resp, err := kvc.Get(ctx, prefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		return nil, errors.Wrap(err, "could not get etcd key")
	}
	if len(resp.Kvs) == 0 {
		return nil, errors.New("no free IP addresses")
	}
	ip := string(resp.Kvs[0].Value)
	return parseIP4(ip), nil
}

func (h *DHCPHandler) leaseIP(ctx context.Context, ip net.IP, nic string, ttl time.Duration) error {
	lease, err := etcd.NewLease(h.client).Grant(ctx, int64(ttl.Seconds()))
	if err != nil {
		return errors.Wrap(err, "could not create new lease")
	}

	freeIPKey := h.prefix + "ips::free::" + ip.String()
	leasedIPKey := h.prefix + "ips::leased::" + ip.String()
	leasedNicKey := h.prefix + "nics::leased::" + nic

	res, err := etcd.NewKV(h.client).Txn(ctx).If(
		// if the ip was previously free
		etcdutil.KeyExists(freeIPKey),
	).Then(
		// Unfree it, and associate it with this nic
		etcd.OpDelete(freeIPKey),
		etcd.OpPut(leasedIPKey, nic, etcd.WithLease(lease.ID)),
		etcd.OpPut(leasedNicKey, ip.String(), etcd.WithLease(lease.ID)),
	).Else(
		// Otherwise, we're _probably_ renewing it, so check that it's currently
		// associated with us
		etcd.OpTxn([]etcd.Cmp{
			etcd.Compare(etcd.Value(leasedIPKey), "=", nic),
			etcd.Compare(etcd.Value(leasedNicKey), "=", ip.String()),
		}, []etcd.Op{
			// And if it is, renew the lease
			etcd.OpPut(leasedIPKey, nic, etcd.WithLease(lease.ID)),
			etcd.OpPut(leasedNicKey, ip.String(), etcd.WithLease(lease.ID)),
		}, nil),
	).Commit()
	if err != nil {
		return errors.Wrap(err, "could not update for leased ip")
	}

	// If we did an else then another else, we failed to renew the lease
	if !res.Succeeded &&
		!res.Responses[0].Response.(*etcdpb.ResponseOp_ResponseTxn).ResponseTxn.Succeeded {
		return errors.New("ip is no longer free")
	}

	return nil
}

func (h *DHCPHandler) revokeLease(ctx context.Context, nic string) error {
	kvc := etcd.NewKV(h.client)
	leasedNicKey := h.prefix + "nics::leased::" + nic
	res, err := kvc.Get(ctx, leasedNicKey)
	if err != nil {
		return errors.Wrap(err, "could not get nic's current lease")
	}
	ip := string(res.Kvs[0].Value)
	leasedIPKey := h.prefix + "ips::leased::" + ip
	freeIPKey := h.prefix + "ips::free::" + ip

	_, err = kvc.Txn(ctx).If(
		etcdutil.KeyExists(leasedIPKey),
		etcdutil.KeyExists(leasedNicKey),
	).Then(
		etcd.OpDelete(leasedIPKey),
		etcd.OpDelete(leasedNicKey),
		etcd.OpPut(freeIPKey, ip),
	).Commit()
	return errors.Wrap(err, "could not delete lease")
}

func parseIP4(raw string) net.IP {
	ip := net.ParseIP(raw)
	if ip == nil {
		return nil
	}
	return ip[12:16]
}
