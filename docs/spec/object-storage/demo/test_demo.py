"""Offline regressions: python3 -m unittest discover -s docs/spec/object-storage/demo -p 'test_*.py'."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

DEMO = Path(__file__).resolve().parent


class DemoTests(unittest.TestCase):
    def shell(self, body, cwd=None, env=None):
        return subprocess.run(['bash', '-c', 'set -euo pipefail\nsource "$1/common.sh"\n' + body,
                               'test', str(DEMO)], cwd=cwd, env=env,
                              text=True, capture_output=True)

    def test_hash_covers_package_contents_and_names(self):
        with tempfile.TemporaryDirectory() as d:
            pkg = Path(d, 'pkg')
            pkg.mkdir()
            source = pkg / 'a.go'
            source.write_text('one')
            first = self.shell('compute_source_hash', cwd=d)
            source.write_text('two')
            second = self.shell('compute_source_hash', cwd=d)
            source.rename(pkg / 'b.go')
            third = self.shell('compute_source_hash', cwd=d)
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertEqual(len({first.stdout, second.stdout, third.stdout}), 3)

    def test_secret_values_stay_out_of_arguments(self):
        body = '''
NS=demo SECRET_NAME=creds CONTEXT=expected
export AWS_ACCESS_KEY_ID=example AWS_SECRET_ACCESS_KEY=sensitive
kube() { printf '%s\\n' "$*" >&2; cat; }
apply_credential_secret accessKey=AWS_ACCESS_KEY_ID secretKey=AWS_SECRET_ACCESS_KEY sessionToken=AWS_SESSION_TOKEN
'''
        result = self.shell(body)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('sensitive', result.stderr)
        data = json.loads(result.stdout)
        self.assertEqual(data['metadata']['namespace'], 'demo')
        self.assertNotIn('sessionToken', data['data'])
        self.assertIn('--server-side', result.stderr)

    def test_old_applied_status_cannot_pass_rotation_wait(self):
        result = self.shell('''
NS=demo CLUSTER_NAME=cluster
kube() {
 case "$*" in
  *'get secret'*) printf '%s' '{"data":{"accessKey":"YQ==","secretKey":"Yg=="}}';;
  *'metadata.uid'*) printf uid;;
  *) printf 'Applied old-fingerprint';;
 esac
}
sleep() { :; }
wait_applied aws creds
''')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Timed out', result.stderr)

    def test_recreate_selects_only_owned_group_datadir_claims(self):
        result = self.shell('''
NS=demo CLUSTER_NAME=cluster
kube() {
 case "$*" in
  *'get marklogiccluster '*) printf uid;;
  *'get marklogicgroups'*) printf '%s' '{"items":[{"metadata":{"ownerReferences":[{"uid":"uid"}]},"spec":{"name":"node"}},{"metadata":{"ownerReferences":[{"uid":"other"}]},"spec":{"name":"other"}}]}';;
  *'get pvc'*) printf '%s' '{"items":[{"metadata":{"name":"datadir-node-0"}},{"metadata":{"name":"datadir-other-0"}},{"metadata":{"name":"datadir-node-backup"}}]}';;
  *) printf '%s\\n' "$*";;
 esac
}
recreate_demo_cluster
''')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('delete pvc datadir-node-0', result.stdout)
        self.assertNotIn('datadir-other', result.stdout)
        self.assertNotIn('datadir-node-backup', result.stdout)
        self.assertIn('--cascade=foreground', result.stdout)

    def test_verifier_redacts_and_uses_custom_admin_secret(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            kubectl = root / 'kubectl'
            kubectl.write_text('''#!/usr/bin/env python3
import json, sys, time
args = " ".join(sys.argv[1:])
assert args.startswith("--context expected "), args
if "get marklogiccluster" in args:
 print(json.dumps({"metadata":{"name":"cluster"},"spec":{"auth":{"secretName":"custom"}}}))
elif "get secret custom" in args:
 print('{"data":{"username":"YWRtaW4=","password":"c2Vuc2l0aXZl"}}')
elif "port-forward" in args:
 print('Forwarding from 127.0.0.1:45678 -> 8002', flush=True)
 time.sleep(30)
else:
 raise SystemExit(1)
''')
            curl = root / 'curl'
            curl.write_text('''#!/usr/bin/env python3
import json,sys
assert "sensitive" not in " ".join(sys.argv)
assert "--digest" in sys.argv
print(json.dumps({"aws":{"access-key":"private-id","secret-key":"ciphertext","session-token":"sensitive-token"}}))
''')
            kubectl.chmod(0o755)
            curl.chmod(0o755)
            env = dict(os.environ, PATH=d + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', str(DEMO / 'verify-credentials.sh'),
                                     'expected', 'demo', 'cluster', 'node', 'aws'],
                                    env=env, text=True, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), {'provider': 'aws', 'configured': True,
                                                         'sessionTokenPresent': True})
            self.assertNotIn('sensitive', result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
