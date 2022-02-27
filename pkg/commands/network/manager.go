package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"reflect"

	"github.com/aquasecurity/libbpfgo"
	"github.com/mrtc0/bouheki/pkg/config"
	log "github.com/mrtc0/bouheki/pkg/log"
)

const (
	MODE_MONITOR uint32 = 0
	MODE_BLOCK   uint32 = 1

	TARGET_HOST      uint32 = 0
	TAREGT_CONTAINER uint32 = 1

	// BPF Map Names
	BOUHEKI_CONFIG_MAP_NAME       = "bouheki_config"
	ALLOWED_V4_CIDR_LIST_MAP_NAME = "allowed_v4_cidr_list"
	ALLOWED_V6_CIDR_LIST_MAP_NAME = "allowed_v6_cidr_list"
	DENIED_V4_CIDR_LIST_MAP_NAME  = "denied_v4_cidr_list"
	DENIED_V6_CIDR_LIST_MAP_NAME  = "denied_v6_cidr_list"
	ALLOWED_UID_LIST_MAP_NAME     = "allowed_uid_list"
	DENIED_UID_LIST_MAP_NAME      = "denied_uid_list"
	ALLOWED_GID_LIST_MAP_NAME     = "allowed_gid_list"
	DENIED_GID_LIST_MAP_NAME      = "denied_gid_list"
	ALLOWED_COMMAND_LIST_MAP_NAME = "allowed_command_list"
	DENIED_COMMAND_LIST_MAP_NAME  = "denied_command_list"

	/*
	   +---------------+---------------+-------------------+-------------------+-------------------+
	   | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12  | 13 | 14 | 15 | 16 | 17 | 18 | 19 | 20 |
	   +---------------+---------------+-------------------+-------------------+-------------------+
	   |      MODE     |     TARGET    | Allow Command Size|  Allow UID Size   | Allow GID Size    |
	   +---------------+---------------+-------------------+-------------------+-------------------+
	*/

	MAP_SIZE                = 20
	MAP_MODE_START          = 0
	MAP_MODE_END            = 4
	MAP_TARGET_START        = 4
	MAP_TARGET_END          = 8
	MAP_ALLOW_COMMAND_INDEX = 8
	MAP_ALLOW_UID_INDEX     = 12
	MAP_ALLOW_GID_INDEX     = 16
)

type Manager struct {
	mod         *libbpfgo.Module
	config      *config.Config
	rb          *libbpfgo.RingBuffer
	cache       map[string][]DomainCache
	dnsResolver DNSResolver
}

type DomainCache struct {
	key     []byte
	mapName string
}

type IPAddress struct {
	address  net.IP
	cidrMask net.IPMask
	key      []byte
}

func (i *IPAddress) isV6address() bool {
	return i.address.To4() == nil
}

func (i *IPAddress) ipAddressToBPFMapKey() []byte {
	ip := net.IPNet{IP: i.address.Mask(i.cidrMask), Mask: i.cidrMask}

	if i.isV6address() {
		i.key = ipv6ToKey(ip)
	} else {
		i.key = ipv4ToKey(ip)
	}

	return i.key
}

type DNSResolver interface {
	Resolve(host string) ([]net.IP, error)
}

type DefaultResolver struct{}

func (r *DefaultResolver) Resolve(host string) ([]net.IP, error) {
	return net.LookupIP(host)
}

func (m *Manager) SetConfigToMap() error {
	if err := m.setConfigMap(); err != nil {
		return err
	}
	if err := m.setAllowedCIDRList(); err != nil {
		return err
	}
	if err := m.setDeniedCIDRList(); err != nil {
		return err
	}
	if err := m.setAllowedDomainList(); err != nil {
		return err
	}
	if err := m.setDeniedDomainList(); err != nil {
		return err
	}
	if err := m.setAllowedCommandList(); err != nil {
		return err
	}
	if err := m.setDeniedCommandList(); err != nil {
		return err
	}
	if err := m.setAllowedUIDList(); err != nil {
		return err
	}
	if err := m.setDeniedUIDList(); err != nil {
		return err
	}
	if err := m.setAllowedGIDList(); err != nil {
		return err
	}
	if err := m.setDeniedGIDList(); err != nil {
		return err
	}
	if err := m.attach(); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Start(eventsChannel chan []byte) error {
	rb, err := m.mod.InitRingBuf("audit_events", eventsChannel)

	if err != nil {
		return err
	}

	rb.Start()
	m.rb = rb

	return nil
}

func (m *Manager) Close() {
	m.rb.Close()
}

func (m *Manager) attach() error {
	programs := []string{"socket_connect"}
	for _, progName := range programs {
		prog, err := m.mod.GetProgram(progName)

		if err != nil {
			return err
		}

		_, err = prog.AttachLSM()
		if err != nil {
			return err
		}

		log.Debug(fmt.Sprintf("%s attached.", progName))
	}

	return nil
}

func (m *Manager) setMode(table *libbpfgo.BPFMap, key []byte) []byte {
	if m.config.IsRestricted() {
		binary.LittleEndian.PutUint32(key[MAP_MODE_START:MAP_MODE_END], MODE_BLOCK)
	} else {
		binary.LittleEndian.PutUint32(key[MAP_MODE_START:MAP_MODE_END], MODE_MONITOR)
	}

	return key
}

func (m *Manager) setTarget(table *libbpfgo.BPFMap, key []byte) []byte {
	if m.config.IsOnlyContainer() {
		binary.LittleEndian.PutUint32(key[MAP_TARGET_START:MAP_TARGET_END], TAREGT_CONTAINER)
	} else {
		binary.LittleEndian.PutUint32(key[MAP_TARGET_START:MAP_TARGET_END], TARGET_HOST)
	}

	return key
}

func (m *Manager) setConfigMap() error {
	configMap, err := m.mod.GetMap(BOUHEKI_CONFIG_MAP_NAME)
	if err != nil {
		return err
	}

	key := make([]byte, MAP_SIZE)

	key = m.setMode(configMap, key)
	key = m.setTarget(configMap, key)

	binary.LittleEndian.PutUint32(key[MAP_ALLOW_COMMAND_INDEX:MAP_ALLOW_COMMAND_INDEX+4], uint32(len(m.config.Network.Command.Allow)))
	binary.LittleEndian.PutUint32(key[MAP_ALLOW_UID_INDEX:MAP_ALLOW_UID_INDEX+4], uint32(len(m.config.Network.UID.Allow)))
	binary.LittleEndian.PutUint32(key[MAP_ALLOW_GID_INDEX:MAP_ALLOW_GID_INDEX+4], uint32(len(m.config.Network.GID.Allow)))

	err = configMap.Update(uint8(0), key)

	if err != nil {
		return err
	}

	return nil
}

func (m *Manager) setAllowedCommandList() error {
	commands, err := m.mod.GetMap(ALLOWED_COMMAND_LIST_MAP_NAME)
	if err != nil {
		return err
	}

	for _, c := range m.config.Network.Command.Allow {
		err = commands.Update(byteToKey([]byte(c)), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setDeniedCommandList() error {
	commands, err := m.mod.GetMap(DENIED_COMMAND_LIST_MAP_NAME)
	if err != nil {
		return err
	}

	for _, c := range m.config.Network.Command.Deny {
		err = commands.Update(byteToKey([]byte(c)), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setAllowedUIDList() error {
	uids, err := m.mod.GetMap(ALLOWED_UID_LIST_MAP_NAME)
	if err != nil {
		return err
	}
	for _, uid := range m.config.Network.UID.Allow {
		err = uids.Update(uintToKey(uid), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setDeniedUIDList() error {
	uids, err := m.mod.GetMap(DENIED_UID_LIST_MAP_NAME)
	if err != nil {
		return err
	}
	for _, uid := range m.config.Network.UID.Deny {
		err = uids.Update(uintToKey(uid), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setAllowedGIDList() error {
	gids, err := m.mod.GetMap(ALLOWED_GID_LIST_MAP_NAME)
	if err != nil {
		return err
	}
	for _, gid := range m.config.Network.GID.Allow {
		err = gids.Update(uintToKey(gid), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setDeniedGIDList() error {
	gids, err := m.mod.GetMap(DENIED_UID_LIST_MAP_NAME)
	if err != nil {
		return err
	}
	for _, gid := range m.config.Network.GID.Deny {
		err = gids.Update(uintToKey(gid), uint8(0))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) setAllowedCIDRList() error {
	for _, addr := range m.config.Network.CIDR.Allow {
		allowedAddress, err := cidrToBPFMapKey(addr)
		if err != nil {
			return err
		}
		if allowedAddress.isV6address() {
			err = m.cidrListUpdate(allowedAddress, ALLOWED_V6_CIDR_LIST_MAP_NAME)
			if err != nil {
				return err
			}
		} else {
			err = m.cidrListUpdate(allowedAddress, ALLOWED_V4_CIDR_LIST_MAP_NAME)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *Manager) setDeniedCIDRList() error {
	for _, addr := range m.config.Network.CIDR.Deny {
		deniedAddress, err := cidrToBPFMapKey(addr)
		if err != nil {
			return err
		}
		if deniedAddress.isV6address() {
			err = m.cidrListUpdate(deniedAddress, DENIED_V6_CIDR_LIST_MAP_NAME)
			if err != nil {
				return err
			}
		} else {
			err = m.cidrListUpdate(deniedAddress, DENIED_V4_CIDR_LIST_MAP_NAME)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *Manager) setAllowedDomainList() error {
	for _, domain := range m.config.Network.Domain.Allow {
		allowedAddresses, err := domainNameToBPFMapKey(domain, m.dnsResolver)
		if err != nil {
			return err
		}

		caches, has := m.cache[domain]
		if has {
			err = m.updateDNSCache(caches, allowedAddresses)
			if err != nil {
				return err
			}
		}

		for _, addr := range allowedAddresses {
			if addr.isV6address() {
				err = m.cidrListUpdate(addr, ALLOWED_V6_CIDR_LIST_MAP_NAME)
				if err != nil {
					return err
				}
				m.cache[domain] = []DomainCache{
					{key: addr.key, mapName: ALLOWED_V6_CIDR_LIST_MAP_NAME},
				}
			} else {
				err = m.cidrListUpdate(addr, ALLOWED_V4_CIDR_LIST_MAP_NAME)
				if err != nil {
					return err
				}
				m.cache[domain] = []DomainCache{
					{key: addr.key, mapName: ALLOWED_V4_CIDR_LIST_MAP_NAME},
				}
			}
		}
	}

	return nil
}

func (m *Manager) setDeniedDomainList() error {
	for _, domain := range m.config.Network.Domain.Deny {
		deniedAddresses, err := domainNameToBPFMapKey(domain, m.dnsResolver)
		if err != nil {
			return err
		}

		caches, has := m.cache[domain]
		if has {
			err = m.updateDNSCache(caches, deniedAddresses)
			if err != nil {
				return err
			}
		}

		for _, addr := range deniedAddresses {
			if addr.isV6address() {
				err = m.cidrListUpdate(addr, DENIED_V6_CIDR_LIST_MAP_NAME)
				if err != nil {
					return err
				}
				m.cache[domain] = []DomainCache{
					{key: addr.key, mapName: DENIED_V6_CIDR_LIST_MAP_NAME},
				}
			} else {
				err = m.cidrListUpdate(addr, DENIED_V4_CIDR_LIST_MAP_NAME)
				if err != nil {
					return err
				}
				m.cache[domain] = []DomainCache{
					{key: addr.key, mapName: DENIED_V4_CIDR_LIST_MAP_NAME},
				}
			}
		}
	}

	return nil
}

func (m *Manager) cidrListDeleteKey(mapName string, key []byte) error {
	cidr_list, err := m.mod.GetMap(mapName)
	if err != nil {
		return err
	}

	if err := cidr_list.DeleteKey(key); err != nil {
		return err
	}
	return nil
}

func (m *Manager) cidrListUpdate(addr IPAddress, mapName string) error {
	cidr_list, err := m.mod.GetMap(mapName)
	if err != nil {
		return err
	}
	err = cidr_list.Update(addr.key, uint8(0))
	if err != nil {
		return err
	}
	return nil
}

// updateDNSCache is update DNS Cache.
//
// If the IP address has been changed as a result of name resolution, remove the old IP address from the eBPF Map. If there are no changes, do nothing.
func (m *Manager) updateDNSCache(caches []DomainCache, addresses []IPAddress) error {
	oldCaches := findOldCache(caches, addresses)
	if len(oldCaches) == 0 {
		return nil
	}

	for _, cache := range oldCaches {
		if err := m.cidrListDeleteKey(cache.mapName, cache.key); err != nil {
			return err
		}
	}

	return nil
}

// findOldCache is check the cache against the result of the new name resolution and return the (old) cache to be removed.
//
// If the IP in the cache is not included in the name resolution result, it will be assumed to be an old IP.
func findOldCache(caches []DomainCache, addresses []IPAddress) []DomainCache {
	oldCaches := []DomainCache{}

	for i, cache := range caches {
		dup := false
		for _, addr := range addresses {
			if reflect.DeepEqual(addr.key, cache.key) {
				dup = true
				break
			}
		}
		if !dup {
			oldCaches = append(oldCaches, caches[i])
		}
	}

	return oldCaches
}

func cidrToBPFMapKey(cidr string) (IPAddress, error) {
	ipaddr := IPAddress{}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return ipaddr, err
	}
	ipaddr.address = n.IP
	ipaddr.cidrMask = n.Mask
	ipaddr.ipAddressToBPFMapKey()
	return ipaddr, nil
}

func domainNameToBPFMapKey(host string, resolver DNSResolver) ([]IPAddress, error) {
	var addrs = []IPAddress{}
	addresses, err := resolver.Resolve(host)
	if err != nil {
		return addrs, err
	}
	for _, addr := range addresses {
		ipaddr := IPAddress{address: addr}
		if ipaddr.isV6address() {
			ipaddr.cidrMask = net.CIDRMask(128, 128)
		} else {
			ipaddr.cidrMask = net.CIDRMask(32, 32)
		}
		ipaddr.ipAddressToBPFMapKey()
		addrs = append(addrs, ipaddr)
	}

	return addrs, nil
}

func ipv4ToKey(n net.IPNet) []byte {
	key := make([]byte, 16)
	prefixLen, _ := n.Mask.Size()

	binary.LittleEndian.PutUint32(key[0:4], uint32(prefixLen))
	copy(key[4:], n.IP)

	return key
}

func ipv6ToKey(n net.IPNet) []byte {
	key := make([]byte, 20)
	prefixLen, _ := n.Mask.Size()

	binary.LittleEndian.PutUint32(key[0:4], uint32(prefixLen))
	copy(key[4:], n.IP)

	return key
}

func byteToKey(b []byte) []byte {
	key := make([]byte, 16)
	copy(key[0:], b)
	return key
}

func uintToKey(i uint) []byte {
	key := make([]byte, 4)
	binary.LittleEndian.PutUint32(key[0:4], uint32(i))
	return key
}
