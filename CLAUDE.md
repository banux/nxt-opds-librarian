# CLAUDE.md

Consignes pour Claude Code sur ce dépôt.

## Versioning — tag obligatoire

**Tout nouveau commit sur `main` doit être suivi d'un tag de version.** Le
self-update (`librarian update`) tire la *dernière release GitHub*, et un push
de tag `v*` déclenche `build.yml` qui publie les binaires de la release. Sans
tag, le commit n'est jamais distribué aux déploiements.

- Schéma : `vMAJ.MIN` (incrément de `MIN` ; dernier en date visible via
  `git tag --sort=-v:refname | head -1`).
- Tag **annoté**, message au format `vX.YY — résumé court du changement`.
- Pousser le commit **et** le tag : `git push origin main --follow-tags`
  (ou `git push origin main && git push origin vX.YY`).
