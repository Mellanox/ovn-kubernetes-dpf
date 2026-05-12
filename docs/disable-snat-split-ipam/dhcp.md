# DHCP Options for Host Interface

Two options are available for providing DHCP to host interfaces.

---

## Option 1: HBN DHCP

Run `dhcpd` directly inside the HBN pod/container.

### Configuration

```bash
cat >/tmp/hbn-dhcpd.conf <<EOF
authoritative;
default-lease-time 3600;
max-lease-time 3600;
log-facility local7;

subnet 10.0.120.0 netmask 255.255.255.254 {
  option routers 10.0.120.0;
  option subnet-mask 255.255.255.254;
  option interface-mtu 1460;

  host worker1-pf {
    # Host PF MAC address
    hardware ethernet $HOST1_PF_MAC;
    fixed-address 10.0.120.1;
  }
}
EOF
```

### Start `dhcpd`

```bash
touch /tmp/hbn-dhcpd.leases /tmp/hbn-dhcpd.pid

dhcpd -4 -d \
  -cf /tmp/hbn-dhcpd.conf \
  -lf /tmp/hbn-dhcpd.leases \
  -pf /tmp/hbn-dhcpd.pid \
  pf2dpu2_if >/tmp/hbn-dhcpd.log 2>&1 &
```

---

## Option 2: HBN DHCP Relay with a standalone DHCP server (simulated DPUService)

A `dnsmasq` instance runs in a dedicated network namespace (simulating a DPUService) and the HBN acts as a DHCP relay.

### Create namespace and veth pair

```bash
ip netns add ns-dhcp
ip link add veth-dhcp type veth peer name veth-hbn
ip link set veth-dhcp netns ns-dhcp
ip link set veth-hbn netns "$HBNPID"
```

### Configure dnsmasq side

```bash
ip netns exec ns-dhcp ip addr add 169.254.169.250/30 dev veth-dhcp
ip netns exec ns-dhcp ip link set veth-dhcp up
ip netns exec ns-dhcp ip route add 10.0.120.2/32 via 169.254.169.249 dev veth-dhcp
```

### Configure HBN-side veth from the DPU host

```bash
nsenter -t "$HBNPID" -n ip link set veth-hbn name relay_if
nsenter -t "$HBNPID" -n ip addr add 169.254.169.249/30 dev relay_if
nsenter -t "$HBNPID" -n ip link set relay_if up
```

### Start dnsmasq in `ns-dhcp`

```bash
ip netns exec ns-dhcp dnsmasq \
  --interface=veth-dhcp \
  --bind-interfaces \
  --dhcp-host=94:6d:ae:00:e9:b4,10.0.120.3 \
  --dhcp-range=10.0.120.3,10.0.120.3,255.255.255.254,12h \
  --dhcp-option=option:router,10.0.120.2 \
  --dhcp-authoritative \
  --port=0 \
  --log-dhcp \
  --log-facility=/tmp/dnsmasq-relay.log \
  --pid-file=/tmp/dnsmasq-relay.pid
```

### On HBN side: start DHCP relay

```bash
dhcrelay -4 -d \
  -i pf2dpu2_if \
  -i relay_if \
  169.254.169.250 >/tmp/hbn-dhcrelay.log 2>&1 &
```

### Equivalent NV config commands

```bash
nv set service dhcp-relay default server 169.254.169.250
nv set service dhcp-relay default interface pf2dpu2_if downstream
nv set service dhcp-relay default interface relay_if upstream
nv set service dhcp-relay default gateway-interface pf2dpu2_if
nv set service dhcp-relay default source-ip giaddress
nv config apply
```

```yaml
vrf:
  default:
    service:
      dhcp-relay:
        default:
          server:
            169.254.169.250: {}
          interface:
            pf2dpu2_if:
              downstream: {}
            relay_if:
              upstream: {}
          gateway-interface:
            pf2dpu2_if:
              address: auto
          source-ip: giaddress
```
