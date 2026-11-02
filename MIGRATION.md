# Migrating

No earlier public version of wr exists: 0.2.1 is the first working release (the v0.2.0 tag never produced assets). It needs mw >= 0.2.1 and board schema 8 (run `mw migrate` on every workspace first, see the mw MIGRATION.md).

## 0.2.5

wr 0.2.4 refused schema 9 boards (written by mw 0.3.0 and later) with "wr needs schema 8: run `mw migrate`". 0.2.5 accepts schema 8 and 9; nothing to migrate. From now on wr and mw install and update together (`wr update` / `mw update` update both to their latest releases), so run `wr update` or `mw update` once with 0.2.5 or 0.3.19 and both stay current.
