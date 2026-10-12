# Folds internal/i18n/parts/*.json into catalog_en.json (sorted; a later part
# wins; disagreements are printed). Run from the repo root:
#   python3 internal/i18n/merge.py [--clean]
import json, glob, os, sys
cat = json.load(open('internal/i18n/catalog_en.json'))
seen = {}
for f in sorted(glob.glob('internal/i18n/parts/*.json')):
    for k, v in json.load(open(f)).items():
        if not isinstance(v, str) or not v.strip():
            continue
        if k in seen and cat.get(k) != v:
            print(f'conflict {k!r}: {seen[k]}={cat[k]!r} vs {f}={v!r}')
        cat[k] = v
        seen[k] = f
json.dump(dict(sorted(cat.items())), open('internal/i18n/catalog_en.json', 'w'), ensure_ascii=False, indent=1)
open('internal/i18n/catalog_en.json', 'a').write('\n')
if '--clean' in sys.argv:
    for f in glob.glob('internal/i18n/parts/*.json'):
        os.remove(f)
print(len(cat), 'entries')
