#!/usr/bin/env python3
"""Mirror the product Web UI into the offline website demo.

`site/demo-product/web/` is a generated snapshot: edit `web/`, then re-run this
script. The copy stays byte-identical apart from index.html, which drops the
product CSP, rewrites absolute asset paths and loads the demo fixtures.
Never copy native credentials or data.
"""
import argparse, re, shutil, subprocess
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--source', type=Path, default=Path('.'))
args = parser.parse_args()

source = args.source.resolve()
site = Path(__file__).resolve().parents[1] / 'site'
out = site / 'demo-product' / 'web'
if out.parent.name != 'demo-product':
    raise SystemExit('unexpected demo target: ' + str(out))
if out.exists():
    shutil.rmtree(out)
shutil.copytree(source / 'web', out)

p = out / 'index.html'
s = p.read_text()
s = re.sub(r'<meta http-equiv="Content-Security-Policy"[^>]*>', '', s)
csp = ("default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "
       "img-src 'self' data: blob:; font-src 'self'; connect-src 'none'; "
       "form-action 'none'; base-uri 'none'; object-src 'none'")
s = s.replace('<head>', '<head><meta http-equiv="Content-Security-Policy" content="' + csp + '">'
              '<script src="../../demo-data/isolation.js"></script>'
              '<script src="../../demo-data/accounts.js"></script>'
              '<script src="../../demo-data/web.js"></script>')
s = re.sub(r'(src|href)="/(?!/)', r'\1="./', s)
s = s.replace('<link rel="manifest" href="./manifest.webmanifest">', '')
s = s.replace('</body>', '<script src="../../demo-data/scenes.js"></script></body>')
p.write_text(s)

mobile_ui = source / 'mobile/android/assets/mobile-ui.js'
if mobile_ui.is_file():
    shutil.copy2(mobile_ui, site / 'demo-data/mobile-ui.js')

print('Demo UI copied from', subprocess.check_output(['git', 'rev-parse', '--short', 'HEAD'], cwd=source, text=True).strip())
