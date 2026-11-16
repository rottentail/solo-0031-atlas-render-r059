#!/usr/bin/env python3
"""Run bounded production flows against a temporary Atlas Render server."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[1]


def sample():
    return {"width": 320, "height": 240, "background": "#F5F1E8", "layers": [
        {"id": "plate", "kind": "rectangle", "width": 220, "height": 150,
         "transform": {"x": 45, "y": 35, "scaleX": 1, "scaleY": 1, "degrees": 5},
         "opacity": 1, "fill": {"kind": "linear", "color": "#145A70", "endColor": "#32A5A0", "from": {"x": 0, "y": 0}, "to": {"x": 220, "y": 150}}},
        {"id": "accent", "kind": "ellipse", "width": 130, "height": 130,
         "transform": {"x": 150, "y": 95, "scaleX": 1, "scaleY": 1, "degrees": 0},
         "opacity": .85, "fill": {"kind": "solid", "color": "#E87E4F"}},
        {"id": "inset", "kind": "group", "transform": {"x": 150, "y": 100, "scaleX": 1, "scaleY": 1, "degrees": -8}, "opacity": .9,
         "clip": {"x": 0, "y": 0, "width": 90, "height": 75}, "children": [
             {"id": "facet", "kind": "polygon", "width": 100, "height": 100,
              "transform": {"scaleX": 1, "scaleY": 1}, "opacity": .8,
              "fill": {"kind": "solid", "color": "#FFF4D5"}, "points": [{"x": 10, "y": 90}, {"x": 50, "y": 0}, {"x": 100, "y": 90}]}]}]}


def tiny_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([240, 40, 80, 255] * 2) + b'\0' + bytes([40, 80, 240, 128] * 2))) + chunk(b'IEND', b'')


class Demo:
    def __init__(self, binary, directory):
        self.binary = binary
        self.directory = directory
        self.proc = None
        self.log = open(directory / 'server.log', 'ab')
        self.start()

    def start(self):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        self.url = f'http://127.0.0.1:{port}'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{port}', ATLAS_STATE=str(self.directory / 'state.json'))
        self.proc = subprocess.Popen([str(self.binary)], env=env, stdout=self.log, stderr=self.log)
        for _ in range(100):
            try:
                self.call('GET', '/healthz')
                return
            except (urllib.error.URLError, ConnectionError):
                if self.proc.poll() is not None:
                    raise RuntimeError((self.directory / 'server.log').read_text())
                time.sleep(.03)
        raise RuntimeError('server startup deadline exceeded')

    def stop(self, abrupt=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if abrupt else self.proc.terminate)()
            self.proc.wait(timeout=15)

    def call(self, method, path, body=None, status=200, headers=None):
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            if response.status != status:
                raise RuntimeError(f'{method} {path}: expected {status}, received {response.status}: {data[:500]!r}')
            if response.headers.get('Content-Type', '').startswith('application/json'):
                return json.loads(data)
            return data

    def wait(self, job_id, wanted=('succeeded',)):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            if job['state'] in ('failed', 'cancelled'):
                raise RuntimeError(f'unexpected job outcome: {job}')
            time.sleep(.025)
        raise RuntimeError('render deadline exceeded')

    def submit(self, snapshot, key='render-one', scale=1):
        return self.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': scale}, 202, {'Idempotency-Key': key})


def run(demo, workflow, out):
    d = sample()
    put = lambda doc, rev=0: demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记', 'expectedRevision': rev, 'document': doc})
    scene = put(d)
    if workflow == 'create-scene':
        assert demo.call('GET', '/v1/scenes/emblem')['document'] == scene['document']
        bad = sample(); bad['layers'][0]['transform']['scaleX'] = 0
        demo.call('PUT', '/v1/scenes/invalid', {'name': '无效几何', 'expectedRevision': 0, 'document': bad}, 400)
    elif workflow == 'revise-scene':
        d['background'] = '#F0F0F0'; assert put(d, 1)['revision'] == 2
        demo.call('PUT', '/v1/scenes/emblem', {'name': '旧版本', 'expectedRevision': 1, 'document': d}, 409)
    elif workflow == 'archive-scene':
        archived = demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': 1, 'state': 'archived'})
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': archived['revision']}, 409)
        assert demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': archived['revision'], 'state': 'active'})['revision'] == 3
    if workflow in ('import-asset', 'retire-asset'):
        asset = demo.call('PUT', '/v1/assets/ribbon', tiny_png(), 201)
        assert asset['width'] == 2 and asset['digest'] == hashlib.sha256(tiny_png()).hexdigest()
        demo.call('PUT', '/v1/assets/broken', b'not png', 400)
        layer = {'id': 'ribbon', 'kind': 'image', 'width': 60, 'height': 60, 'assetId': 'ribbon', 'opacity': 1, 'transform': {'x': 20, 'y': 30, 'scaleX': 1, 'scaleY': 1}}
        d['layers'].append(layer); put(d, 1)
        pinned = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 201)
        demo.call('POST', '/v1/assets/ribbon/retire')
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 409)
        result = demo.wait(demo.submit(pinned)['id']); assert result['state'] == 'succeeded'
        return {'workflow': workflow, 'assetDigest': asset['digest'], 'pinnedRender': result['state']}
    current = demo.call('GET', '/v1/scenes/emblem')
    snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision']}, 201)
    if workflow == 'freeze-scene':
        original_digest = snapshot['digest']; d['background'] = '#000000'; put(d, current['revision'])
        assert demo.call('GET', '/v1/snapshots/' + snapshot['id'])['digest'] == original_digest
    if workflow in ('export-package', 'import-package'):
        package = demo.call('GET', '/v1/snapshots/' + snapshot['id'] + '/package')
        restored = demo.call('POST', '/v1/imports/restored', {'name': '恢复的印记', 'package': package}, 201)
        assert restored['document'] == snapshot['document']
        corrupt = json.loads(json.dumps(package)); corrupt['snapshot']['document']['width'] += 1
        demo.call('POST', '/v1/imports/corrupt', {'name': '损坏包', 'package': corrupt}, 400)
        demo.call('GET', '/v1/scenes/corrupt', status=404)
    if workflow in ('cancel-render', 'retry-render', 'recover-render'):
        heavy = sample(); heavy['width'] = 1500; heavy['height'] = 1500
        heavy['layers'] = [dict(heavy['layers'][0], id=f'sheet-{i}', width=1500, height=1500, transform={'scaleX': 1, 'scaleY': 1}) for i in range(16)]
        put(heavy, current['revision'])
        snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
    job = demo.submit(snapshot)
    if workflow == 'submit-render':
        assert demo.submit(snapshot)['id'] == job['id']
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 2}, 409, {'Idempotency-Key': 'render-one'})
    if workflow == 'revoke-snapshot':
        demo.call('POST', '/v1/snapshots/' + snapshot['id'] + '/revoke')
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 1}, 409, {'Idempotency-Key': 'blocked-after-revoke'})
    if workflow in ('cancel-render', 'retry-render'):
        stopped = demo.call('POST', '/v1/jobs/' + job['id'] + '/cancel')
        assert stopped['state'] == 'cancelled'
        if workflow == 'cancel-render':
            time.sleep(.1); assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'cancelled'
            return {'workflow': workflow, 'state': 'cancelled'}
        job = demo.call('POST', '/v1/jobs/' + job['id'] + '/retry', status=202)
        assert job['attempt'] == 2
    if workflow == 'recover-render':
        demo.wait(job['id'], wanted=('running',)); demo.stop(abrupt=True); demo.start()
    result = demo.wait(job['id'])
    png = demo.call('GET', '/v1/artifacts/' + result['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n'
    assert struct.unpack('>II', png[16:24]) == (snapshot['document']['width'], snapshot['document']['height'])
    if out:
        Path(out).mkdir(parents=True, exist_ok=True); (Path(out) / 'atlas-sample.png').write_bytes(png)
    if workflow == 'compare-renders':
        other = demo.wait(demo.submit(snapshot, 'render-two')['id'])
        comparison = demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': other['artifactId']})
        assert comparison['changedPixels'] == 0
        d['background'] = '#000000'; put(d, current['revision'])
        altered = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
        changed = demo.wait(demo.submit(altered, 'render-three')['id'])
        assert demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': changed['artifactId']})['changedPixels'] > 0
    if workflow == 'purge-artifact':
        demo.call('DELETE', '/v1/artifacts/' + result['artifactId'])
        demo.call('GET', '/v1/artifacts/' + result['artifactId'], status=410)
        assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    if workflow == 'retention-drill':
        return retention_drill(demo, d, put, current, snapshot, result, job)
    if workflow == 'retention-expiry':
        short = demo.call('POST', '/v1/retention/previews', {'artifactIds': [result['artifactId']]}, 201)
        time.sleep(1.3)
        expired = demo.call('POST', '/v1/retention/previews/' + short['id'] + '/execute', {'token': short['token']}, 409)
        assert expired['reason'] == 'expired'
        assert demo.call('GET', '/v1/artifacts/' + result['artifactId'])[:8] == b'\x89PNG\r\n\x1a\n'
        return {'workflow': workflow, 'previewState': short['state'], 'refusal': expired['reason']}
    if workflow == 'observe-service':
        summary = demo.call('GET', '/v1/status'); assert summary['jobs']['succeeded'] >= 1
        events = demo.call('GET', '/v1/events'); assert any(e['kind'] == 'job.succeeded' for e in events)
    return {'workflow': workflow, 'state': result['state'], 'pngBytes': len(png), 'pngDigest': hashlib.sha256(png).hexdigest()}


def retention_drill(demo, document, put, current, snapshot, result, job):
    """Preview, conflict, concurrent-read and restart drill for result cleanup."""
    import threading
    first = result['artifactId']
    other = demo.wait(demo.submit(snapshot, 'drill-one')['id'])['artifactId']
    assert first != other
    # A preview reports impact without deleting anything.
    preview = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first], 'note': 'drill'}, 201)
    assert preview['state'] == 'pending' and preview['eligibleCount'] == 1
    assert preview['items'][0]['jobState'] == 'succeeded' and preview['bytesReclaimable'] > 0
    token = preview['token']
    demo.call('GET', '/v1/artifacts/' + first)
    # A download after the preview makes it stale: the bytes may be in use.
    stale = demo.call('POST', '/v1/retention/previews/' + preview['id'] + '/execute', {'token': token}, 409)
    assert stale['reason'] == 'access-changed' and stale['artifactIds'] == [first]
    # A tampered token is rejected even for a fresh preview.
    fresh = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first]}, 201)
    bad = demo.call('POST', '/v1/retention/previews/' + fresh['id'] + '/execute', {'token': 'not-the-token'}, 409)
    assert bad['reason'] == 'token-mismatch'
    # "before" selects live results completed before the cutoff; the preview
    # reports the impact of the whole set without deleting any of it.
    cutoff = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() + 1))
    sweep = demo.call('POST', '/v1/retention/previews', {'before': cutoff}, 201)
    assert sweep['eligibleCount'] >= 2 and all(item['jobState'] == 'succeeded' for item in sweep['items'])
    demo.call('GET', '/v1/artifacts/' + first)  # reading invalidates this plan
    demo.call('POST', '/v1/retention/previews/' + sweep['id'] + '/execute', {'token': sweep['token']}, 409)
    # A pixel comparison counts as a read and invalidates an earlier preview.
    pair = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first, other]}, 201)
    demo.call('POST', '/v1/compare', {'left': first, 'right': other})
    compared = demo.call('POST', '/v1/retention/previews/' + pair['id'] + '/execute', {'token': pair['token']}, 409)
    assert compared['reason'] == 'access-changed' and set(compared['artifactIds']) == {first, other}
    # A new completed result invalidates an earlier plan, even though no result
    # was read in the meantime.
    fresh = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first]}, 201)
    third = demo.wait(demo.submit(snapshot, 'drill-two')['id'])['artifactId']
    changed = demo.call('POST', '/v1/retention/previews/' + fresh['id'] + '/execute', {'token': fresh['token']}, 409)
    assert changed['reason'] == 'results-changed'
    # A restart invalidates pending previews; the preview and job facts survive.
    pending = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first]}, 201)
    demo.stop(abrupt=False); demo.start()
    after = demo.call('GET', '/v1/retention/previews/' + pending['id'])
    assert after['state'] == 'stale'
    restarted = demo.call('POST', '/v1/retention/previews/' + pending['id'] + '/execute', {'token': pending['token']}, 409)
    assert restarted['reason'] == 'restarted'
    assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    # A fresh post-restart preview can purge; a same-token replay is idempotent.
    ready = demo.call('POST', '/v1/retention/previews', {'artifactIds': [first]}, 201)
    executed = demo.call('POST', '/v1/retention/previews/' + ready['id'] + '/execute', {'token': ready['token']})
    assert executed['state'] == 'executed' and executed['purged'][0]['jobState'] == 'succeeded'
    replay = demo.call('POST', '/v1/retention/previews/' + ready['id'] + '/execute', {'token': ready['token']})
    assert replay['replayed'] is True and replay['bytesReleased'] == executed['bytesReleased']
    gone = demo.call('GET', '/v1/artifacts/' + first, status=410)
    assert gone['jobState'] == 'succeeded' and gone['digest'] == executed['purged'][0]['digest'] and gone['purged'] is True
    assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    # Concurrent download and purge: readers must always receive a complete
    # body (whole PNG or a well-formed 410), and a preview taken before a read
    # must never authorize removing bytes that read is using.
    target = other
    errors = []
    started = threading.Event()
    request = urllib.request.Request(demo.url + '/v1/artifacts/' + target)
    def reader():
        # Bounded burst: readers hammer the result while the operator tries to
        # purge it, then stop so one final attempt can succeed.
        for _ in range(120):
            try:
                with urllib.request.urlopen(request, timeout=15) as response:
                    body = response.read()
                    if response.status == 200 and body[:8] != b'\x89PNG\r\n\x1a\n':
                        errors.append('truncated PNG body')
                    started.set()
            except urllib.error.HTTPError as error:
                body = error.read()
                if error.code != 410 or json.loads(body).get('jobState') != 'succeeded':
                    errors.append(f'unexpected status {error.code}: {body[:200]!r}')
            except Exception as error:  # noqa: BLE001 - surfaced through errors
                errors.append(repr(error))
    threads = [threading.Thread(target=reader) for _ in range(6)]
    for thread in threads: thread.start()
    assert started.wait(5), 'reader never completed a download'
    refusals = set()
    plan = demo.call('POST', '/v1/retention/previews', {'artifactIds': [target]}, 201)
    # A read completed after the preview makes it stale: bytes may be in use.
    demo.call('GET', '/v1/artifacts/' + target)
    stale = demo.call('POST', '/v1/retention/previews/' + plan['id'] + '/execute', {'token': plan['token']}, 409)
    assert stale['reason'] == 'access-changed' and stale['artifactIds'] == [target], stale
    refusals.add(stale['reason'])
    outcome = None
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        plan = demo.call('POST', '/v1/retention/previews', {'artifactIds': [target]}, 201)
        try:
            outcome = demo.call('POST', '/v1/retention/previews/' + plan['id'] + '/execute', {'token': plan['token']}, 200)
            break
        except RuntimeError as error:
            reasons = [reason for reason in ('artifact-in-use', 'access-changed') if reason in str(error)]
            assert '409' in str(error) and reasons, error
            refusals.update(reasons)
    for thread in threads:
        thread.join(timeout=20)
    assert not errors, errors
    assert outcome is not None, 'concurrent purge never completed'
    assert 'access-changed' in refusals or 'artifact-in-use' in refusals, refusals
    demo.call('GET', '/v1/artifacts/' + target, status=410)
    # The compatibility entry and a remaining result still round-trip.
    compat = demo.call('DELETE', '/v1/artifacts/' + third)
    assert compat['state'] == 'purged' and compat['execution']['purged'][0]['state'] == 'purged'
    again = demo.call('DELETE', '/v1/artifacts/' + third)
    assert again['execution']['purged'][0]['state'] == 'already-purged' and again['execution']['bytesReleased'] == 0
    status = demo.call('GET', '/v1/status')
    assert status['purgedResultCount'] >= 3 and status['artifactBytes'] == 0
    events = demo.call('GET', '/v1/events')
    kinds = [e['kind'] for e in events]
    assert 'retention.previewed' in kinds and 'retention.executed' in kinds and 'artifact.purged' in kinds
    return {'workflow': 'retention-drill', 'previewId': ready['id'], 'purged': 3,
            'jobState': 'succeeded', 'replayed': replay['replayed'], 'concurrentRefusals': sorted(refusals)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workflow', default='submit-render')
    parser.add_argument('--preview-ttl', help='ATLAS_RETENTION_PREVIEW_TTL for the drill server')
    parser.add_argument('--out')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='atlas-walkthrough-') as temp:
        directory = Path(temp); binary = directory / 'atlas'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/atlas'], cwd=ROOT, check=True, timeout=60)
        if args.preview_ttl:
            os.environ['ATLAS_RETENTION_PREVIEW_TTL'] = args.preview_ttl
        demo = Demo(binary, directory)
        try:
            print(json.dumps(run(demo, args.workflow, args.out), ensure_ascii=False))
        finally:
            demo.stop(); demo.log.close()


if __name__ == '__main__':
    main()
