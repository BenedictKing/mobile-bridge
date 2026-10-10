"""Self-heal the ZCode local state the headless wrapper reads from disk.

Two defects the zcode-acp-server wrapper inherits from
``<zcode-home>/v2/config.json``, both fixed here:

1. The coding-plan key slot. The ZCode desktop app in OAuth family mode
   stores the OAuth access JWT in
   ``builtin:bigmodel-coding-plan.options.apiKey`` — overwriting it at
   sign-in, token refresh and account switches. The wrapper serves that slot
   to the backend as the plan credential for every model request on the
   ``account:bigmodel-individual-coding-plan`` provider; client signing
   cannot parse a JWT (``Client signing credential must contain one
   separator``) and every turn dies as a misleading
   ``[网络异常，正在重试 (n/5)…]`` loop. The real ``id.secret`` plan key lives
   AES-256-GCM-encrypted (``enc:v1:`` prefix) in ``v2/credentials.json``
   under ``account-provider:coding-plan:...:account:<uid>:api-key``; the
   healer decrypts the ACTIVE account's key and restores it, so account
   switches converge within one poll interval.

2. The provider/model dropdown. The wrapper builds its model list from
   config.json's legacy ``builtin:*`` entries, which stopped syncing with the
   desktop: the list shows stale models (GLM-5.2/GLM-5-Turbo) and misses the
   plan's current lineup (GLM-5.3-Flash), and a not-held start-plan lingers
   as a duplicate provider because a leftover JWT satisfies the enabled+key
   selectability rule. The healer mirrors the desktop app's own truth: the
   bundled provider table's ``builtinModelIds`` for the held plan, and the
   coding-plan cache's availability verdict for the ghost.

Decryption runs through a one-shot ``node -e`` (Node is already a hard
prerequisite of the zcode agent); the bridge itself stays stdlib-only.
"""
import json
import logging
import os
import re
import shutil
import subprocess
import sys
import threading
import time
from pathlib import Path

LOG = logging.getLogger(__name__)

CONFIG_PROVIDER = 'builtin:bigmodel-coding-plan'
START_PLAN_PROVIDER = 'builtin:bigmodel-start-plan'
ACCOUNT_RULE_ID = 'account:bigmodel-individual-coding-plan'
ACCOUNT_KEY_PREFIX = 'account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:'
USER_INFO_KEY = 'oauth:bigmodel:user_info'
CREDENTIAL_SECRET_ENV = 'ZCODE_CREDENTIAL_SECRET'
CACHE_NAME = 'coding-plan-cache.json'
# The desktop app's bundled provider table, newest truth for what a plan
# offers (same file the wrapper derives its account push from).
TABLE_CANDIDATES = (
    '/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json',
    '~/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json',
    '/opt/ZCode/resources/config/provider/zcode-builtin.json',
    '/usr/share/zcode/resources/config/provider/zcode-builtin.json',
)
# A valid plan credential is exactly apiKeyId.secret; anything else in the
# slot that is not the app's JWT overwrite is a deliberate choice — keep it.
ID_SECRET_RE = re.compile(r'^[^.\s]+\.[^.\s]+$')
POLL_SECONDS = 2.0


# Reads {value, secret} as JSON on stdin, prints the decrypted plaintext.
# Ciphertext and secret never touch argv (visible in `ps`).
NODE_DECRYPT = """
const crypto = require('crypto');
let input = '';
process.stdin.on('data', c => input += c);
process.stdin.on('end', () => {
  try {
    const {value, secret} = JSON.parse(input);
    const [a, b, c] = value.slice('enc:v1:'.length).split('.');
    const key = crypto.createHash('sha256').update(secret).digest();
    const d = crypto.createDecipheriv('aes-256-gcm', key, Buffer.from(a, 'base64url'));
    d.setAuthTag(Buffer.from(b, 'base64url'));
    process.stdout.write(Buffer.concat([d.update(Buffer.from(c, 'base64url')), d.final()])
      .toString('utf8'));
  } catch (e) { process.exit(1); }
});
"""


def zcode_home():
    """Mirror the wrapper's zcodeHomeDir(): $ZCODE_HOME replaces ~/.zcode."""
    explicit = os.environ.get('ZCODE_HOME')
    if explicit:
        return Path(explicit)
    return Path.home() / '.zcode'


def _node_home():
    """os.homedir() as Node computes it: $HOME/$USERPROFILE, else passwd."""
    env = os.environ.get('HOME') or os.environ.get('USERPROFILE')
    if env:
        return env
    try:
        import pwd
        return pwd.getpwuid(os.getuid()).pw_dir
    except (ImportError, KeyError):
        return '~'


def _node_username():
    """userInfo().username as Node computes it: passwd, never the env."""
    try:
        import pwd
        return pwd.getpwuid(os.getuid()).pw_name
    except (ImportError, KeyError):
        return os.environ.get('USER') or os.environ.get('LOGNAME') or 'unknown'


def _fallback_secret():
    """Replicate Node's defaultCredentialSecret() byte for byte:
    zcode-credential-fallback:<platform>:<homedir>:<username>."""
    return 'zcode-credential-fallback:{}:{}:{}'.format(sys.platform, _node_home(), _node_username())


def _secret():
    return (os.environ.get(CREDENTIAL_SECRET_ENV) or '').strip() or _fallback_secret()


def _decrypt(value, secret, node):
    """Decrypt one enc:v1 value; None on any failure (never raises)."""
    if not isinstance(value, str) or not value.startswith('enc:v1:'):
        return value
    try:
        result = subprocess.run(
            [node, '-e', NODE_DECRYPT],
            input=json.dumps({'value': value, 'secret': secret}),
            capture_output=True, text=True, timeout=20)
    except (OSError, subprocess.SubprocessError) as exc:
        LOG.debug('zcode key heal: node decrypt failed: %s', exc)
        return None
    if result.returncode != 0:
        return None
    return result.stdout


def _looks_like_jwt(value):
    return isinstance(value, str) and value.startswith('eyJ') and value.count('.') == 2


def _read_json(path):
    try:
        with open(path, encoding='utf-8') as handle:
            return json.load(handle)
    except (OSError, ValueError):
        return None


def _plan_keys(creds):
    """All stored bigmodel coding-plan api-key entries: {account id: enc value}."""
    out = {}
    for name, value in (creds or {}).items():
        if isinstance(value, str) and name.startswith(ACCOUNT_KEY_PREFIX) and name.endswith(':api-key'):
            out[name[len(ACCOUNT_KEY_PREFIX):-len(':api-key')]] = value
    return out


def resolve_plan_key(creds_path=None, node=None):
    """(decrypted id.secret, account id) for the active account, or (None, None)."""
    node = node or shutil.which('node')
    if not node:
        return None, None
    creds_path = Path(creds_path) if creds_path else zcode_home() / 'v2' / 'credentials.json'
    creds = _read_json(creds_path)
    keys = _plan_keys(creds)
    if not keys:
        return None, None
    secret = _secret()
    account = None
    user_info = _decrypt(creds.get(USER_INFO_KEY), secret, node)
    if user_info:
        try:
            account = json.loads(user_info).get('id')
        except ValueError:
            account = None
    if account not in keys:
        # Stale or absent user_info: unambiguous only with a single account.
        account = next(iter(keys)) if len(keys) == 1 else None
    if account is None:
        return None, None
    plan_key = _decrypt(keys[account], secret, node)
    if plan_key and ID_SECRET_RE.match(plan_key):
        return plan_key, account
    return None, None


def find_builtin_table():
    for candidate in TABLE_CANDIDATES:
        path = Path(os.path.expanduser(candidate))
        if path.is_file():
            return path
    return None


def _table_plan_models(table_path):
    """builtinModelIds the bundled table declares for the held plan, or None."""
    data = _read_json(table_path) if table_path else None
    rules = (((data or {}).get('config') or {}).get('providerConfigRules') or {}).get('providerRules') or []
    for rule in rules:
        if rule.get('providerId') == ACCOUNT_RULE_ID:
            ids = ((rule.get('config') or {}).get('builtinModelIds'))
            if isinstance(ids, list) and ids:
                return [str(model_id) for model_id in ids]
    return None


def _cache_status(cache_path, provider_id):
    data = _read_json(cache_path) if cache_path else None
    items = (((data or {}).get('entryStatus') or {}).get('items') or {})
    return (items.get(provider_id) or {}).get('status')


def _sync_models(config, table_path, changes):
    """Mirror the plan's builtinModelIds onto the coding-plan entry.

    Existing per-model specs are preserved (reasoning variants, priorities);
    new models get a bare entry — the backend resolves a model's real options
    at session time, the config entry only has to list it. Models the plan no
    longer offers are dropped: the desktop's own dropdown does not show them,
    and a switch to one would either fail or bill outside the plan.
    """
    if not table_path:
        return
    provider = (config.get('provider') or {}).get(CONFIG_PROVIDER)
    if not isinstance(provider, dict):
        return
    plan_ids = _table_plan_models(table_path)
    if not plan_ids:
        return
    models = provider.get('models')
    models = models if isinstance(models, dict) else {}
    if list(models.keys()) == plan_ids:
        return
    provider['models'] = {model_id: (models.get(model_id) or {}) for model_id in plan_ids}
    changes.append('models -> {}'.format(','.join(plan_ids)))


def _disable_ghost_start_plan(config, cache_path, changes):
    """Hide a start-plan the account does not hold.

    A leftover OAuth JWT satisfies the enabled+key selectability rule, so the
    wrapper advertises a duplicate provider whose every turn would die on the
    Aliyun-captcha boundary (#123) — and its stale model list is where the
    phantom GLM-5.2/GLM-5-Turbo entries came from. Only an explicit
    'unavailable' verdict from the desktop's own cache plus a non-plan key
    triggers the disable; a held plan or a real pasted key is left alone.
    """
    provider = (config.get('provider') or {}).get(START_PLAN_PROVIDER)
    if not isinstance(provider, dict) or provider.get('enabled') is not True:
        return
    key = ((provider.get('options') or {}).get('apiKey')) or ''
    if ID_SECRET_RE.match(key):
        return
    if _cache_status(cache_path, START_PLAN_PROVIDER) != 'unavailable':
        return
    provider['enabled'] = False
    changes.append('start-plan disabled (not entitled)')


def ensure_zcode_state(config_path=None, creds_path=None, node=None,
                       table_path=None, cache_path=None):
    """One healing pass: plan key, model lineup, ghost start-plan.

    Returns a status word for tests and logging: ok (nothing to do), patched
    (config rewritten), custom (slot holds a deliberate foreign key — only
    the non-key parts may still have been applied), unavailable (nothing
    servable — no node, no stored key, unreadable config).
    """
    home = zcode_home()
    config_path = Path(config_path) if config_path else home / 'v2' / 'config.json'
    creds_path = Path(creds_path) if creds_path else home / 'v2' / 'credentials.json'
    cache_path = Path(cache_path) if cache_path else home / 'v2' / CACHE_NAME
    table_path = find_builtin_table() if table_path is None else (Path(table_path) if table_path else None)
    node = node or shutil.which('node')
    config = _read_json(config_path)
    if not config or not node:
        return 'unavailable'
    providers = config.get('provider')
    if not isinstance(providers, dict) or not isinstance(providers.get(CONFIG_PROVIDER), dict):
        return 'unavailable'

    key_status = 'unavailable'
    try:
        slot = providers[CONFIG_PROVIDER]['options']['apiKey']
    except (KeyError, TypeError):
        slot = None
    plan_key, account = resolve_plan_key(creds_path, node)
    if plan_key and slot == plan_key:
        key_status = 'ok'
    elif plan_key:
        # Rewrite when the app overwrote the slot (JWT), left it empty, or a
        # previous heal went stale after an account switch (slot holds another
        # stored account's key). A foreign hand-written key is respected.
        stored_keys = set()
        for value in _plan_keys(_read_json(creds_path)).values():
            decrypted = _decrypt(value, _secret(), node)
            if decrypted:
                stored_keys.add(decrypted)
        if not slot or _looks_like_jwt(slot) or slot in stored_keys:
            providers[CONFIG_PROVIDER].setdefault('options', {})['apiKey'] = plan_key
            key_status = 'patched'
        else:
            key_status = 'custom'
    elif slot is None or _looks_like_jwt(slot) or slot == '':
        LOG.warning('zcode: coding-plan key slot holds %s but no stored plan key '
                    'could be decrypted; re-login in the ZCode app if turns fail',
                    'a JWT' if slot else 'nothing')

    changes = []
    if key_status == 'patched':
        changes.append('plan key (account {})'.format(account))
    _sync_models(config, table_path, changes)
    _disable_ghost_start_plan(config, cache_path, changes)
    if changes:
        _atomic_write_json(config_path, config)
        LOG.info('zcode: healed %s in %s (%s)', ', '.join(changes), config_path, key_status)
        return 'patched'
    return key_status


def _atomic_write_json(path, payload):
    tmp = path.with_name(path.name + '.bridge-tmp')
    with open(tmp, 'w', encoding='utf-8') as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2)
        handle.write('\n')
    os.replace(tmp, path)


_watcher = None


def _stamp(*paths):
    stamps = []
    for path in paths:
        try:
            stat = Path(path).stat()
            stamps.append((stat.st_mtime_ns, stat.st_size))
        except OSError:
            stamps.append(None)
    return tuple(stamps)


def _watch_loop(paths, interval):
    signature = _stamp(*paths)
    while True:
        time.sleep(interval)
        current = _stamp(*paths)
        if current == signature:
            continue
        signature = current
        try:
            ensure_zcode_state()
        except Exception:  # never let the healer kill the gateway
            LOG.debug('zcode state heal: check raised', exc_info=True)


def ensure_and_watch():
    """One synchronous heal plus a background watcher; safe to call repeatedly.

    The watcher re-heals when any input changes: an account switch in the
    desktop app (new JWT in config.json, new active entry in
    credentials.json), a plan refresh (coding-plan-cache.json) or an app
    update (bundled provider table) all converge back to a consistent state.
    """
    global _watcher
    try:
        ensure_zcode_state()
    except Exception:
        LOG.debug('zcode state heal: initial check raised', exc_info=True)
    if _watcher is None or not _watcher.is_alive():
        home = zcode_home()
        paths = [home / 'v2' / 'config.json', home / 'v2' / 'credentials.json',
                 home / 'v2' / CACHE_NAME]
        table = find_builtin_table()
        if table:
            paths.append(table)
        _watcher = threading.Thread(
            target=_watch_loop, args=(paths, POLL_SECONDS),
            daemon=True, name='zcode-creds-heal')
        _watcher.start()
