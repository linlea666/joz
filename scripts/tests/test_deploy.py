"""Subprocess tests: real deployment scripts, fake Docker/git; no live service."""
import fcntl
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
FAKE = '''#!/usr/bin/env python3
import fcntl, json, os, pathlib, shutil, sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ['TEST_ROOT'])
with open(root/'commands','a') as f:f.write(name+' '+ ' '.join(args)+'\\n')
if name=='flock':
 try:fcntl.flock(int(args[-1]), fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError:sys.exit(1)
elif name=='git':
 if args[:1]==['-C']:args=args[2:]
 if args[:2]==['remote','get-url']:print(os.getenv('TEST_REMOTE','https://github.com/linlea666/joz.git'))
 elif args[:2]==['branch','--show-current']:print(os.getenv('TEST_BRANCH','main'))
 elif args[:1]==['status']:print(os.getenv('TEST_DIRTY',''),end='')
 elif args[:2]==['rev-parse','--show-toplevel']:print(root.resolve())
 elif args[:1]==['rev-parse']:print('testcommit')
 elif args[:1]==['merge-base'] and os.getenv('TEST_DIVERGED'):sys.exit(1)
 elif args[:1]==['merge']:
  p=root/'start.sh';s=p.read_text();s=s.replace('set -euo pipefail', 'set -euo pipefail\\nprintf reexecuted >> "'+str(root/'reexec')+'"',1);p.write_text(s)
elif name=='docker':
 if args[:1]==['compose']:
  if 'build' in args and os.getenv('TEST_BUILD_FAIL')==args[-1]:sys.exit(1)
  if 'up' in args:
   if os.getenv('TEST_UP_FAIL'):sys.exit(1)
   (root/'running').touch()
  if 'ps' in args:
   if '-aq' in args and (root/'running').exists():print('container')
   elif '-q' in args:print(args[-1])
  if 'exec' in args:print('Collector IPC: healthy; Discord: waiting_config')
 elif args[:1]==['inspect']:print(os.getenv('TEST_HEALTH','healthy'))
'''

class DeployTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name)
        for file in ['start.sh','install.sh','.env.example','docker-compose.yml']:
            shutil.copy(ROOT/file,self.root/file)
        shutil.copytree(ROOT/'scripts',self.root/'scripts',ignore=shutil.ignore_patterns('tests','__pycache__'))
        (self.root/'.git').mkdir();(self.root/'bin').mkdir()
        for name in ['git','docker','flock']:
            p=self.root/'bin'/name;p.write_text(FAKE);p.chmod(0o755)
        self.env=dict(os.environ,TEST_ROOT=str(self.root),PATH=str(self.root/'bin')+':'+os.environ['PATH'])
    def run_start(self,*args,ok=True,**env):
        result=subprocess.run(['bash',str(self.root/'start.sh'),*args],cwd='/',env=dict(self.env,**env),capture_output=True,text=True,timeout=15)
        self.assertEqual(result.returncode==0,ok,result.stdout+result.stderr)
        return result
    def commands(self):return (self.root/'commands').read_text()
    def test_fresh_repeat_and_sequential_build(self):
        # Existing server ports outside the test are irrelevant; obtain free ports.
        ports=[]
        for _ in range(2):
            with socket.socket() as s:s.bind(('127.0.0.1',0));ports.append(s.getsockname()[1])
        template=(self.root/'.env.example').read_text().replace('=3000','= '+str(ports[0])).replace('=8080','= '+str(ports[1]))
        (self.root/'.env.example').write_text(template)
        self.run_start('start')
        env=(self.root/'.env').read_bytes()
        self.assertEqual((self.root/'.env').stat().st_mode & 0o777,0o600)
        self.assertEqual((self.root/'data').stat().st_mode & 0o777,0o700)
        (self.root/'data'/'database.db').write_text('preserved')
        self.run_start('start','--build')
        self.assertEqual((self.root/'.env').read_bytes(),env)
        self.assertEqual((self.root/'data'/'database.db').read_text(),'preserved')
        log=self.commands();self.assertLess(log.index('build nofx\n'),log.index('build nofx-frontend'));self.assertLess(log.index('build discord-collector'),log.index('up -d'))
        self.assertNotIn('down',log)
    def test_fast_forward_reexec_preserves_private_files(self):
        (self.root/'running').touch()
        self.run_start('start')
        private=(self.root/'.env').read_text()+'\nSMTP_PASS=test-only-secret\n'
        (self.root/'.env').write_text(private)
        (self.root/'data'/'db').write_text('run_mode=live;email=db;buffer=keep')
        self.run_start('update')
        self.assertTrue((self.root/'reexec').exists())
        self.assertEqual((self.root/'.env').read_text(),private)
        self.assertIn('run_mode=live',(self.root/'data'/'db').read_text())
        self.assertNotIn('test-only-secret',self.commands())
    def test_update_rejects_wrong_repository_branch_changes_and_divergence(self):
        for env in [dict(TEST_REMOTE='https://github.com/other/repo'),dict(TEST_BRANCH='dev'),dict(TEST_DIRTY=' M start.sh'),dict(TEST_DIVERGED='1')]:
            self.run_start('update',ok=False,**env)
        self.assertNotIn('build nofx',self.commands())
    def test_concurrent_deploy_is_rejected(self):
        with open(self.root/'.git/nofx-deploy.lock','w') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            self.run_start('start',ok=False)
        self.assertNotIn('build nofx',self.commands())
    def test_build_failure_does_not_touch_running_services(self):
        (self.root/'running').touch()
        self.run_start('start',ok=False,TEST_BUILD_FAIL='nofx-frontend')
        self.assertNotIn(' up ',self.commands());self.assertNotIn(' stop',self.commands());self.assertTrue((self.root/'running').exists())
    def test_start_failure_and_unhealthy_service_are_not_success(self):
        (self.root/'running').touch()
        for env in [dict(TEST_UP_FAIL='1'),dict(TEST_HEALTH='unhealthy')]:
            result=self.run_start('start',ok=False,**env)
            self.assertNotIn('部署验收通过',result.stdout)
    def test_occupied_port_is_rejected_before_build(self):
        with socket.socket() as s:
            s.bind(('0.0.0.0',0));s.listen()
            (self.root/'.env.example').write_text((self.root/'.env.example').read_text().replace('NOFX_FRONTEND_PORT=3000','NOFX_FRONTEND_PORT='+str(s.getsockname()[1])))
            self.run_start('start',ok=False)
        self.assertNotIn('build nofx',self.commands())
    def test_update_never_initializes_missing_env_or_keys(self):
        self.run_start('update',ok=False)
        self.assertFalse((self.root/'.env').exists())
        (self.root/'.env').write_text('JWT_SECRET=incomplete\n')
        self.run_start('update',ok=False)
        self.assertEqual((self.root/'.env').read_text(),'JWT_SECRET=incomplete\n')
    def test_missing_key_with_existing_data_is_not_regenerated(self):
        (self.root/'data').mkdir();(self.root/'data'/'db').write_text('existing')
        self.run_start('start',ok=False)
        self.assertFalse((self.root/'.env').exists())

if __name__=='__main__':unittest.main()
