# ext4 Specification Audit

Objectif: rapprocher l'implémentation Go de la spécification ext4 du kernel
(https://docs.kernel.org/filesystems/ext4/) et fournir des tests automatisés
vérifiant les points de conformité.

Procédé:
- Écrire une check-list de points de conformité prioritaires
- Ajouter des tests qui exposent les écarts
- Corriger l'implémentation pour faire passer les tests
- Répéter jusqu'à couverture satisfaisante

Priorité initiale (ordre de travail):

1. Journaling & replay
   - Vérifier que les transactions sont appliquées dans l'ordre attendu
   - Assurer que l'application via le dispatcher respecte l'ordre per-file+group
   - Tests: commit/replay correctness, crash-recovery simulated replay
   - Status: in-progress (commit dispatcher + direct journal apply implemented)

2. Metadata checksums (metadata_csum)
   - Vérifier calculs CRC32c pour bitmaps, BGD, superblock
   - Tests: generate images with metadata_csum feature and validate csum fields
   - Status: todo

3. Extents support
   - Vérifier format d'extent, lookup et allocation
   - Tests: create files with extents, read/write validation vs expected block maps
   - Status: todo

4. Directory indexing (htree)
   - Vérifier htree creation/search semantics and hash compatibility
   - Tests: large dir inserts + lookup correctness
   - Status: todo

5. Block group descriptor layout & descriptor size handling
   - Support for various `s_desc_size` values; read/write descriptor table
   - Tests: images with varying desc sizes
   - Status: todo

6. Inode table & inode size/fields
   - Verify inode layout, link counts, flags (extents, etc.)
   - Tests: inode field round-trips, link/unlink semantics
   - Status: todo

7. Extended attributes (xattr) and ACLs
   - Support retrieval and storage of xattrs and POSIX ACLs where relevant
   - Tests: set/get xattr and ACL round-trips
   - Status: todo

8. Online resize (Grow) behavior
   - Verify addition of block groups, bgd table updates, and consistency
   - Tests: Grow() then validate fs structures
   - Status: todo

9. Feature flags & compatibility
   - Ensure proper handling of compat/ro-compat/incompat flags (flex_bg, metadata_csum, 64bit, etc.)
   - Tests: create images with feature combinations and validate behavior
   - Status: todo

10. Cross-validation with e2fsprogs (where available)
    - Run `mke2fs`/`e2fsck` on generated images to validate semantics
    - Tests: optional, run in environments with tools available
    - Status: todo

Execution immédiate:
- Automatiser cette check-list comme tâches unitaires dans `pkg/go-filesystems/ext4/test`.
- Commencer par écrire tests pour `1. Journaling & replay` (already partially covered).

Notes:
- Certains tests nécessitent `-tags test` (tracing, test helpers) and/or external tools.
- Prioriser tests rapides et in-process (no external binaries) where possible.


