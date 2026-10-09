"""Installer OS/package branches with synthetic Ubuntu files and command stubs.
This is NOT a substitute for the real Ubuntu Docker smoke job.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[2]
MOCK='''#!/usr/bin/env python3
import fcntl, os, pathlib, shutil, socket, sys
args=sys.argv[1:];name=pathlib.Path(sys.argv[0]).name
base=pathlib.Path(os.environ['TEST_BASE']);target=base/'nofx'
with open(base/'commands','a') as f:f.write(name+' '+' '.join(args)+'\\n')
if name=='id':print(os.getenv('TEST_UID','0'))
elif name=='uname':print(os.getenv('TEST_ARCH','x86_64'))
elif name=='df':print('Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/test 100000000 0 '+os.getenv('TEST_DISK','100000000')+' 0% /')
elif name=='flock':
 try:fcntl.flock(int(args[-1]),fcntl.LOCK_EX|fcntl.LOCK_NB)
 except BlockingIOError:sys.exit(1)
elif name=='curl':pathlib.Path(args[args.index('-o')+1]).write_text('fake key')
elif name=='git':
 if args[:1]==['-C']:args=args[2:]
 if args[:1]==['clone']:
  target.mkdir(exist_ok=True);(target/'.git').mkdir();src=pathlib.Path(os.environ['TEST_SOURCE'])
  for file in ['start.sh','.env.example','docker-compose.yml']:shutil.copy(src/file,target/file)
  shutil.copytree(src/'scripts',target/'scripts',ignore=shutil.ignore_patterns('tests','__pycache__'))
  template=(target/'.env.example').read_text()
  for port in ['3000','8080']:
   with socket.socket() as sock:
    sock.bind(('127.0.0.1',0));template=template.replace('='+port,'='+str(sock.getsockname()[1]))
  (target/'.env.example').write_text(template)
 elif args[:2]==['remote','get-url']:print(os.getenv('TEST_REMOTE','https://github.com/linlea666/joz.git'))
 elif args[:2]==['branch','--show-current']:print(os.getenv('TEST_BRANCH','main'))
 elif args[:2]==['rev-parse','--show-toplevel']:print(target.resolve())
 elif args[:1]==['rev-parse']:print('testcommit')
 elif args[:1]==['status']:print(os.getenv('TEST_DIRTY',''),end='')
elif name=='docker':
 if args[:1]==['inspect']:print('healthy')
 if args[:1]==['compose']:
  if 'ps' in args and '-q' in args:print(args[-1])
  if 'exec' in args:print('Collector IPC healthy; waiting_config')
elif name=='apt-get':
 if 'docker-ce' in args:
  shutil.copy(base/'bin'/'apt-get',base/'bin'/'docker')
'''

class InstallerTests(unittest.TestCase):
 def setUp(self):
  temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup);self.base=Path(temp.name)
  (self.base/'bin').mkdir();(self.base/'apt/sources.list.d').mkdir(parents=True)
  (self.base/'os-release').write_text('ID=ubuntu\nVERSION_ID=24.04\n')
  (self.base/'meminfo').write_text('MemTotal: 4000000 kB\n')
  script=(ROOT/'install.sh').read_text().replace('/etc/os-release',str(self.base/'os-release')).replace('/proc/meminfo',str(self.base/'meminfo')).replace('/etc/apt/',str(self.base/'apt')+'/')
  (self.base/'install.sh').write_text(script)
  for name in ['id','uname','df','flock','curl','git','docker','apt-get','systemctl']:
   p=self.base/'bin'/name;p.write_text(MOCK);p.chmod(0o755)
  for name in ['bash','python3','openssl','dirname','mkdir','awk','chmod','install','find','cat']:
   (self.base/'bin'/name).symlink_to(shutil.which(name))
  self.env=dict(os.environ,TEST_BASE=str(self.base),TEST_SOURCE=str(ROOT),PATH=str(self.base/'bin'))
 def run_install(self,ok=True,**env):
  result=subprocess.run(['bash',str(self.base/'install.sh'),str(self.base/'nofx')],env=dict(self.env,**env),capture_output=True,text=True,timeout=15)
  self.assertEqual(result.returncode==0,ok,result.stdout+result.stderr)
  return result
 def log(self):return (self.base/'commands').read_text()
 def test_fresh_with_docker_and_repeated_install(self):
  self.run_install();env=(self.base/'nofx/.env').read_bytes()
  self.run_install();self.assertEqual((self.base/'nofx/.env').read_bytes(),env)
  self.assertEqual(self.log().count('git clone'),1)
  self.assertNotIn('docker-ce',self.log())
 def test_missing_docker_installs_official_packages(self):
  (self.base/'bin/docker').unlink()
  self.run_install()
  self.assertIn('download.docker.com/linux/ubuntu/gpg',self.log())
  self.assertIn('docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin',self.log())
  self.assertIn('Suites: noble',(self.base/'apt/sources.list.d/docker.sources').read_text())
 def test_unsupported_os_arch_permissions_and_resources(self):
  for env in [dict(TEST_ARCH='aarch64'),dict(TEST_UID='1000'),dict(TEST_DISK='100')]:self.run_install(ok=False,**env)
  (self.base/'meminfo').write_text('MemTotal: 1000 kB\n');self.run_install(ok=False)
  (self.base/'os-release').write_text('ID=ubuntu\nVERSION_ID=22.04\n');self.run_install(ok=False)
  self.assertNotIn('apt-get',self.log())
 def test_existing_foreign_directory_is_not_touched(self):
  (self.base/'nofx').mkdir();sentinel=self.base/'nofx/keep';sentinel.write_text('untouched')
  self.run_install(ok=False);self.assertEqual(sentinel.read_text(),'untouched');self.assertNotIn('apt-get',self.log())
 def test_existing_checkout_branch_or_origin_is_rejected(self):
  self.run_install()
  self.run_install(ok=False,TEST_REMOTE='https://github.com/other/repo')
  self.run_install(ok=False,TEST_BRANCH='dev')
  self.run_install(ok=False,TEST_DIRTY=' M file')

if __name__=='__main__':unittest.main()
