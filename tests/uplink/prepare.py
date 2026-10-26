#!/usr/bin/env python3
"""Freeze production sources with transport seams and isolated peer startup sizes."""
import hashlib
import json
from pathlib import Path
import shutil
import sys

ROOT = Path(__file__).resolve().parents[2]
DEST = Path(sys.argv[1]).resolve()
DEST.mkdir(parents=True, exist_ok=True)


def replace_once(source, old, new):
    if source.count(old) != 1:
        raise SystemExit(f"Production seam changed; review harness replacement: {old!r}")
    return source.replace(old, new, 1)


module = replace_once((ROOT / 'go.mod').read_text(),
                      'module www.bamsoftware.com/git/dnstt.git', 'module controlled')
module += '\nrequire www.bamsoftware.com/git/dnstt.git v0.0.0\n'
snapshot = DEST / 'source'
snapshot.mkdir(exist_ok=True)
shutil.copy(ROOT / 'go.mod', snapshot / 'go.mod')
shutil.copy(ROOT / 'go.sum', snapshot / 'go.sum')
# The main fixture module owns replace directives, including the minimal
# smux patch. Freeze its source both beside that module and in the snapshot.
for parent in (DEST, snapshot):
    shutil.copytree(ROOT / 'third_party' / 'smux', parent / 'third_party' / 'smux', dirs_exist_ok=True)
for directory in ('bbr', 'fairqueue', 'cake', 'dns', 'noise', 'turbotunnel', 'diagnostics'):
    (snapshot / directory).mkdir(exist_ok=True)
    for path in (ROOT / directory).glob('*.go'):
        if not path.name.endswith('_test.go'):
            shutil.copy(path, snapshot / directory / path.name)

# Both peers share this test process, whereas deployed peers each have their
# own startup queue-size global. Freeze the startup sizes into each fixture
# connection and remote map so scheduling cannot make one peer borrow the
# other's setting. These constructors exist only in the copied test module.
path = snapshot / 'turbotunnel/remotemap.go'
source = replace_once(path.read_text(),
                     'func NewRemoteMap(timeout time.Duration) *RemoteMap {',
                     'func NewRemoteMap(timeout time.Duration) *RemoteMap {\n'
                     ' return NewRemoteMapSized(timeout, GetQueueSize())\n}\n'
                     'func NewRemoteMapSized(timeout time.Duration, size int) *RemoteMap {')
source = replace_once(source, 'type remoteMapInner struct {',
                      'type remoteMapInner struct {\n queueSize int')
source = replace_once(source, 'inner: remoteMapInner{',
                      'inner: remoteMapInner{\n queueSize: size,')
source = replace_once(source, 'size := GetQueueSize()',
                      'size := inner.queueSize')
path.write_text(source)
path = snapshot / 'turbotunnel/queuepacketconn.go'
source = replace_once(path.read_text(),
                     'func NewQueuePacketConn(localAddr net.Addr, timeout time.Duration) *QueuePacketConn {',
                     'func NewQueuePacketConn(localAddr net.Addr, timeout time.Duration) *QueuePacketConn {\n'
                     ' return NewQueuePacketConnSized(localAddr, timeout, GetQueueSize())\n}\n'
                     'func NewQueuePacketConnSized(localAddr net.Addr, timeout time.Duration, size int) *QueuePacketConn {')
source = replace_once(source, 'NewRemoteMap(timeout)', 'NewRemoteMapSized(timeout, size)')
source = replace_once(source, 'make(chan taggedPacket, GetQueueSize())',
                      'make(chan taggedPacket, size)')
path.write_text(source)
module += 'replace www.bamsoftware.com/git/dnstt.git => ' + json.dumps(str(snapshot)) + '\n'
(DEST / 'go.mod').write_text(module)
shutil.copy(ROOT / 'go.sum', DEST / 'go.sum')
for role in ('client', 'server'):
    target = DEST / role
    target.mkdir(exist_ok=True)
    for path in (ROOT / ('dnstt-' + role)).glob('*.go'):
        if path.name.endswith('_test.go'):
            continue
        source = replace_once(path.read_text(), 'package main', 'package ' + role)
        if path.name == 'dns.go' and role == 'client':
            source = replace_once(source, 'turbotunnel.NewQueuePacketConn(clientID, 0)',
                                  'turbotunnel.NewQueuePacketConnSized(clientID, 0, benchQueueSize())')
        if path.name == 'main.go' and role == 'client':
            source = replace_once(source, 'net.ListenTCP("tcp", localAddr)',
                                  'benchListen("tcp", localAddr)')
        if path.name == 'main.go' and role == 'server':
            source = replace_once(source,
                                  'turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, idleTimeout*2)',
                                  'turbotunnel.NewQueuePacketConnSized(turbotunnel.DummyAddr{}, idleTimeout*2, 2*kcpWindow)')
            source = replace_once(source, 'dialer := net.Dialer{\n\t\tTimeout: upstreamDialTimeout,\n\t}', '')
            source = replace_once(source, 'dialer.Dial("tcp", upstream)', 'benchDial("tcp", upstream)')
            source = replace_once(source, 'upstreamConn.(*net.TCPConn)', 'upstreamConn.(halfCloseConn)')
            source = replace_once(source, 'case ch <- &record{resp, addr, clientID, advertised}:\n\t\t\tdefault:',
                                  'case ch <- &record{resp, addr, clientID, advertised}:\n\t\t\tdefault:\n droppedRecords.Add(1)')
        (target / path.name).write_text(source)

(DEST / 'client' / 'bench.go').write_text('''package client
import("net"; "www.bamsoftware.com/git/dnstt.git/dns"; "www.bamsoftware.com/git/dnstt.git/turbotunnel")
var benchListen func(string,*net.TCPAddr)(net.Listener,error)
func benchQueueSize()int{if bbrOn{return 2*bbrMaxWindow};return 2*kcpWindow}
func Start(p net.PacketConn, ln net.Listener, pub []byte, mtu int, adaptive, fair bool) error {
 bbrOn=adaptive; fairUpload=fair; kcpWindow=1024; streamBuffer=16777216; smuxRecvBuf=33554432
 benchListen=func(string,*net.TCPAddr)(net.Listener,error){return ln,nil}
 domain,_:=dns.ParseName("t.example")
 dc:=NewDNSPacketConn(p,turbotunnel.DummyAddr{},domain,65535,true)
 return run(pub,domain,nil,turbotunnel.DummyAddr{},dc,mtu)
}
''')
(DEST / 'server' / 'bench.go').write_text('''package server
import("net"; "os"; "strconv"; "sync/atomic"; "www.bamsoftware.com/git/dnstt.git/dns")
type halfCloseConn interface {net.Conn; CloseRead()error; CloseWrite()error}
var droppedRecords atomic.Int64
func Dropped()int64{return droppedRecords.Load()}
var benchDial func(string,string)(net.Conn,error)
func Start(p net.PacketConn, key []byte, dial func(string,string)(net.Conn,error))error{
 fairDownload=os.Getenv("SERVER_FAIR")=="1"; kcpWindow=1024; if v:=os.Getenv("SERVER_WINDOW");v!=""{kcpWindow,_=strconv.Atoi(v)}; benchDial=dial
 domain,_:=dns.ParseName("t.example")
 return run(key,domain,"memory",p,16384)
}
''')
shutil.copy(ROOT / 'tests/uplink/main_test.go.tmpl', DEST / 'main_test.go')
files = [ROOT / 'go.mod', ROOT / 'go.sum']
files.extend((ROOT / 'third_party' / 'smux').glob('*'))
for directory in ('dnstt-client', 'dnstt-server', 'bbr', 'fairqueue', 'cake', 'dns', 'noise', 'turbotunnel', 'diagnostics'):
    files.extend(path for path in (ROOT / directory).glob('*.go') if not path.name.endswith('_test.go'))
files.extend((ROOT / 'tests/uplink').glob('*.*'))
(DEST / 'source-hashes.json').write_text(json.dumps(
    {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in files if path.is_file()},
    indent=2) + '\n')
