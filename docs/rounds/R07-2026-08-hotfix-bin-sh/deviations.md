# Deviations of round R07 — hotfix /bin/sh

## Deviation 1 — /bin/bash não criado
- **Planned:** symlinks /bin/sh e /bin/bash.
- **Implemented:** apenas /bin/sh.
- **Reason:** busybox não tem applet bash — chamado como bash responde "applet not found" e sai com erro; sem o link, o kubectl recebe ENOENT limpo e cai para /bin/sh. Link quebrado é pior que ausente.
- **Decision registered at:** comentário de aceite da issue #28.
- **Reference document updated:** Dockerfile (stage shell-links).
