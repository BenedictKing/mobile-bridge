"""ZCode local-state self-heal: key slot, model lineup, ghost start-plan."""
import json
import os
import shutil
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from bridge.agents import zcode_creds

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)

NODE = shutil.which('node')
SECRET = 'test-secret'
PLAN_KEY_A = 'id-aaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbb'
PLAN_KEY_B = 'id-cccccccccccccccccccc.ddddddddddddddddddddddddddddddd'
JWT = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4'

NODE_ENCRYPT = """
const crypto = require('crypto');
let input = '';
process.stdin.on('data', c => input += c);
process.stdin.on('end', () => {
  const {value, secret} = JSON.parse(input);
  const key = crypto.createHash('sha256').update(secret).digest();
  const iv = crypto.randomBytes(12);
  const c = crypto.createCipheriv('aes-256-gcm', key, iv);
  const ct = Buffer.concat([c.update(value, 'utf8'), c.final()]);
  process.stdout.write('enc:v1:' + [iv, c.getAuthTag(), ct]
    .map(b => b.toString('base64url')).join('.'));
});
"""


def enc(value):
    import subprocess
    out = subprocess.run([NODE, '-e', NODE_ENCRYPT],
                         input=json.dumps({'value': value, 'secret': SECRET}),
                         capture_output=True, text=True, timeout=20)
    assert out.returncode == 0, out.stderr
    return out.stdout


def write_config(path, api_key, models=None, start_plan=None):
    """Coding-plan provider with `models` ({id: spec}); optional start-plan row."""
    provider = {'builtin:bigmodel-coding-plan': {
        'name': 'BigModel - Coding Plan', 'enabled': True,
        'options': {'apiKey': api_key,
                    'baseURL': 'https://open.bigmodel.cn/api/anthropic'}}}
    if models is not None:
        provider['builtin:bigmodel-coding-plan']['models'] = models
    if start_plan is not None:
        provider['builtin:bigmodel-start-plan'] = dict(
            {'name': 'BigModel- Coding Plan', 'enabled': True,
             'options': {'apiKey': start_plan,
                         'baseURL': 'https://zcode.z.ai/api/v1/zcode-plan/anthropic'}},
            **({'models': {'GLM-5.3': {}, 'GLM-5-Turbo': {}}} if start_plan else {}))
    path.write_text(json.dumps({'provider': provider}), encoding='utf-8')


def write_creds(path, accounts, active=None):
    creds = {f'{zcode_creds.ACCOUNT_KEY_PREFIX}{uid}:api-key': enc(key)
             for uid, key in accounts.items()}
    if active:
        creds[zcode_creds.USER_INFO_KEY] = enc(json.dumps({'id': active}))
    path.write_text(json.dumps(creds), encoding='utf-8')


def write_table(path, plan_models):
    path.write_text(json.dumps({'schemaVersion': 1, 'revision': 30, 'config': {
        'providerConfigRules': {'providerRules': [
            {'providerId': zcode_creds.ACCOUNT_RULE_ID,
             'config': {'access': {'type': 'zhipu-account',
                                   'accountType': 'bigmodel',
                                   'mode': 'individual-coding-plan'},
                        'builtinModelIds': plan_models}},
        ]}}}), encoding='utf-8')


def write_cache(path, statuses):
    path.write_text(json.dumps({'version': 1, 'entryStatus': {'items': {
        pid: {'status': status} for pid, status in statuses.items()}}}), encoding='utf-8')


def read_slot(path):
    return json.loads(path.read_text(encoding='utf-8'))['provider'][
        'builtin:bigmodel-coding-plan']['options']['apiKey']


def read_models(path):
    return json.loads(path.read_text(encoding='utf-8'))['provider'][
        'builtin:bigmodel-coding-plan'].get('models', {})


def read_start_plan(path):
    return json.loads(path.read_text(encoding='utf-8'))['provider'].get(
        'builtin:bigmodel-start-plan')


@unittest.skipIf(NODE is None, 'node is required for enc:v1 round-trips')
class HealTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.config = self.dir / 'config.json'
        self.creds = self.dir / 'credentials.json'
        self.table = self.dir / 'table.json'
        self.cache = self.dir / CACHE_NAME if (CACHE_NAME := 'coding-plan-cache.json') else None
        # Pin the cipher secret so the fixtures are deterministic and the
        # real machine's credential store is never consulted. No table/cache
        # fixtures unless a test writes them: key-only healing by default.
        env = patch.dict(os.environ, {zcode_creds.CREDENTIAL_SECRET_ENV: SECRET})
        env.start()
        self.addCleanup(env.stop)

    def heal(self, table='skip', cache=None):
        return zcode_creds.ensure_zcode_state(
            self.config, self.creds, NODE,
            table_path=self.table if table == 'use' else (table if table else False),
            cache_path=cache if cache else self.cache)

    def test_jwt_replaced_with_active_key(self):
        write_config(self.config, JWT)
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')
        self.assertEqual(self.heal(), 'patched')
        self.assertEqual(read_slot(self.config), PLAN_KEY_A)

    def test_already_healed_is_untouched(self):
        write_config(self.config, PLAN_KEY_A)
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')
        before = self.config.read_text(encoding='utf-8')
        self.assertEqual(self.heal(), 'ok')
        self.assertEqual(self.config.read_text(encoding='utf-8'), before)

    def test_custom_key_respected(self):
        write_config(self.config, 'sk-my-own-key')
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')
        self.assertEqual(self.heal(), 'custom')
        self.assertEqual(read_slot(self.config), 'sk-my-own-key')

    def test_empty_slot_filled(self):
        write_config(self.config, '')
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')
        self.assertEqual(self.heal(), 'patched')
        self.assertEqual(read_slot(self.config), PLAN_KEY_A)

    def test_active_account_picks_right_key(self):
        write_config(self.config, JWT)
        write_creds(self.creds, {'111': PLAN_KEY_A, '222': PLAN_KEY_B}, active='222')
        self.assertEqual(self.heal(), 'patched')
        self.assertEqual(read_slot(self.config), PLAN_KEY_B)

    def test_account_switch_reheals_stale_key(self):
        write_config(self.config, PLAN_KEY_A)
        write_creds(self.creds, {'111': PLAN_KEY_A, '222': PLAN_KEY_B}, active='111')
        self.assertEqual(self.heal(), 'ok')
        # Account switch: the app rewrites user_info (and maybe the slot).
        write_creds(self.creds, {'111': PLAN_KEY_A, '222': PLAN_KEY_B}, active='222')
        write_config(self.config, JWT)
        self.assertEqual(self.heal(), 'patched')
        self.assertEqual(read_slot(self.config), PLAN_KEY_B)
        # A later switch that only rewrites the JWT, leaving our key stale:
        write_creds(self.creds, {'111': PLAN_KEY_A, '222': PLAN_KEY_B}, active='111')
        self.assertEqual(self.heal(), 'patched')
        self.assertEqual(read_slot(self.config), PLAN_KEY_A)

    def test_ambiguous_accounts_without_user_info(self):
        write_config(self.config, JWT)
        write_creds(self.creds, {'111': PLAN_KEY_A, '222': PLAN_KEY_B})
        self.assertEqual(self.heal(), 'unavailable')
        self.assertEqual(read_slot(self.config), JWT)

    def test_wrong_secret_leaves_slot_alone(self):
        write_config(self.config, JWT)
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')
        with patch.dict(os.environ, {zcode_creds.CREDENTIAL_SECRET_ENV: 'other'}):
            self.assertEqual(self.heal(), 'unavailable')
        self.assertEqual(read_slot(self.config), JWT)

    def test_missing_creds_unavailable(self):
        write_config(self.config, JWT)
        self.assertEqual(self.heal(), 'unavailable')
        self.assertEqual(read_slot(self.config), JWT)


@unittest.skipIf(NODE is None, 'node is required for enc:v1 round-trips')
class ModelSyncTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.config = self.dir / 'config.json'
        self.creds = self.dir / 'credentials.json'
        self.table = self.dir / 'table.json'
        self.cache = self.dir / 'coding-plan-cache.json'
        env = patch.dict(os.environ, {zcode_creds.CREDENTIAL_SECRET_ENV: SECRET})
        env.start()
        self.addCleanup(env.stop)
        write_creds(self.creds, {'111': PLAN_KEY_A}, active='111')

    def heal(self):
        return zcode_creds.ensure_zcode_state(
            self.config, self.creds, NODE, table_path=self.table, cache_path=self.cache)

    def test_models_mirrored_from_table(self):
        write_config(self.config, PLAN_KEY_A,
                     models={'GLM-5.3': {'reasoning': {'enabled': True}},
                             'GLM-5.2': {}, 'GLM-5-Turbo': {}})
        write_table(self.table, ['GLM-5.3', 'GLM-5.3-Flash'])
        self.assertEqual(self.heal(), 'patched')
        models = read_models(self.config)
        self.assertEqual(list(models), ['GLM-5.3', 'GLM-5.3-Flash'])
        # Existing specs survive the mirror; the new model gets a bare entry.
        self.assertEqual(models['GLM-5.3'], {'reasoning': {'enabled': True}})
        self.assertEqual(models['GLM-5.3-Flash'], {})

    def test_models_already_in_order_untouched(self):
        write_config(self.config, PLAN_KEY_A,
                     models={'GLM-5.3': {'reasoning': {'enabled': True}},
                             'GLM-5.3-Flash': {}})
        write_table(self.table, ['GLM-5.3', 'GLM-5.3-Flash'])
        before = self.config.read_text(encoding='utf-8')
        self.assertEqual(self.heal(), 'ok')
        self.assertEqual(self.config.read_text(encoding='utf-8'), before)

    def test_missing_table_leaves_models(self):
        write_config(self.config, PLAN_KEY_A, models={'GLM-5.2': {}})
        self.assertEqual(zcode_creds.ensure_zcode_state(
            self.config, self.creds, NODE, table_path=False, cache_path=self.cache), 'ok')
        self.assertEqual(list(read_models(self.config)), ['GLM-5.2'])

    def test_ghost_start_plan_disabled(self):
        write_config(self.config, PLAN_KEY_A, models={'GLM-5.3': {}},
                     start_plan=JWT)
        write_table(self.table, ['GLM-5.3', 'GLM-5.3-Flash'])
        write_cache(self.cache, {'builtin:bigmodel-start-plan': 'unavailable'})
        self.assertEqual(self.heal(), 'patched')
        self.assertIs(read_start_plan(self.config)['enabled'], False)

    def test_held_start_plan_kept(self):
        write_config(self.config, PLAN_KEY_A, models={'GLM-5.3': {}, 'GLM-5.3-Flash': {}},
                     start_plan=JWT)
        write_table(self.table, ['GLM-5.3', 'GLM-5.3-Flash'])
        write_cache(self.cache, {'builtin:bigmodel-start-plan': 'available'})
        self.assertEqual(self.heal(), 'ok')
        self.assertIs(read_start_plan(self.config)['enabled'], True)

    def test_real_start_plan_key_kept(self):
        write_config(self.config, PLAN_KEY_A, models={'GLM-5.3': {}, 'GLM-5.3-Flash': {}},
                     start_plan=PLAN_KEY_B)
        write_table(self.table, ['GLM-5.3', 'GLM-5.3-Flash'])
        write_cache(self.cache, {'builtin:bigmodel-start-plan': 'unavailable'})
        self.assertEqual(self.heal(), 'ok')
        self.assertIs(read_start_plan(self.config)['enabled'], True)


@unittest.skipIf(NODE is None, 'node is required for enc:v1 round-trips')
class WatcherTests(unittest.TestCase):
    def test_watcher_heals_after_app_overwrite(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        home = Path(tmp.name)
        config, creds = home / 'v2' / 'config.json', home / 'v2' / 'credentials.json'
        (home / 'v2').mkdir(parents=True)
        write_creds(creds, {'111': PLAN_KEY_A}, active='111')
        write_config(config, PLAN_KEY_A)
        env = patch.dict(os.environ, {'ZCODE_HOME': str(home),
                                      zcode_creds.CREDENTIAL_SECRET_ENV: SECRET})
        env.start()
        self.addCleanup(env.stop)
        no_table = patch.object(zcode_creds, 'find_builtin_table', return_value=None)
        no_table.start()
        self.addCleanup(no_table.stop)
        zcode_creds._watcher = None  # drop any watcher bound to earlier paths
        zcode_creds.ensure_and_watch()
        self.assertEqual(read_slot(config), PLAN_KEY_A)
        # The desktop app overwrites the slot (sign-in / account switch).
        write_config(config, JWT)
        deadline = time.time() + 15
        while read_slot(config) == JWT and time.time() < deadline:
            time.sleep(0.5)
        self.assertEqual(read_slot(config), PLAN_KEY_A)
