import importlib.util,json,os,shutil,subprocess,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
SPEC=importlib.util.spec_from_file_location('prepare_context',Path(__file__).with_name('prepare_context.py'))
context=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(context)
class PackagingTests(unittest.TestCase):
 def test_tampered_archive_refused_before_extraction(self):
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);bad=root/'bad.tgz';bad.write_bytes(b'untrusted')
   with self.assertRaises(ValueError):context.prepare(bad,root/'context')
   self.assertFalse((root/'context').exists())
   r=subprocess.run(['node',str(ROOT/'deploy/ccloud-runtime/prepare.mjs'),str(bad),str(root/'package')],capture_output=True,text=True)
   self.assertNotEqual(r.returncode,0);self.assertFalse((root/'package').exists())
 def test_context_rejects_extra_files_and_symlinks(self):
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);(root/'runtime.tgz').write_bytes(b'bad')
   with self.assertRaises(ValueError):context.check(root)
   (root/'extra').touch()
   with self.assertRaises(ValueError):context.check(root)
   linked=root/'linked';linked.symlink_to(root,target_is_directory=True)
   with self.assertRaises(ValueError):context.check(linked)
 def test_standard_api_recipe_fails_before_docker_when_artifact_absent(self):
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);bin_dir=root/'bin';bin_dir.mkdir();marker=root/'docker-called'
   fake=bin_dir/'docker';fake.write_text('#!/bin/sh\ntouch "'+str(marker)+'"\nexit 99\n');fake.chmod(0o755)
   env={**os.environ,'PATH':str(bin_dir)+os.pathsep+os.environ['PATH']}
   r=subprocess.run(['make','docker-build-api','CCLOUD_RUNTIME_CONTEXT='+str(root/'missing'),'CCLOUD_NODE_BASE_IMAGE=explicit-test-image'],cwd=ROOT,env=env,capture_output=True,text=True)
   self.assertNotEqual(r.returncode,0);self.assertFalse(marker.exists());self.assertIn('ccloud-runtime-prepare',r.stderr)
 def test_web_and_migrate_recipe_do_not_require_ccloud_context(self):
  for target in ['docker-build-web','docker-build-migrate']:
   r=subprocess.run(['make','-n',target],cwd=ROOT,capture_output=True,text=True,check=True)
   self.assertNotIn('--build-context',r.stdout);self.assertNotIn('prepare_context.py',r.stdout)
 def test_audited_real_bundle(self):
  archive=os.environ.get('CCLOUD_TEST_ARCHIVE')
  if not archive:self.skipTest('set CCLOUD_TEST_ARCHIVE to run audited real bundle verification')
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);context.prepare(Path(archive),root/'context');context.check(root/'context')
   r=subprocess.run(['node',str(ROOT/'deploy/ccloud-runtime/prepare.mjs'),str(root/'context/runtime.tgz'),str(root/'package')],check=True,capture_output=True,text=True)
   result=json.loads(r.stdout);self.assertEqual(result['files'],3447);self.assertEqual(result['version'],'2.2.1')
   bundle=root/'package/ccloud-cli';self.assertEqual((bundle/'BUNDLE.json').stat().st_mode&0o222,0)
   self.assertTrue((bundle/'dist/ccloud-cli.js').is_file())
   self.assertFalse(any(p.is_symlink() for p in bundle.rglob('*')))
if __name__=='__main__':unittest.main()
