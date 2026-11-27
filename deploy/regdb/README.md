# regulatory.db for the OpenStick kernel

`kstick-setup install` installs these as `/lib/firmware/regulatory.db` (via update-alternatives, as `regulatory.db-sforshee`).

The OpenStick 6.6 kernel (`CONFIG_CFG80211_REQUIRE_SIGNED_REGDB=y`) has only one built-in regdb key, Seth Forshee's
(`sforshee`). Upstream wireless-regdb has been signed by Chen-Yu Tsai (`wens`) since 2024.01.23, and Debian's
`wireless-regdb` package signs its copy with a Debian key, so the kernel rejects both: `iw reg reload` fails with -61 (ENODATA)
and Wi-Fi stays in the world domain (`00`). These files are the last sforshee-signed release, **wireless-regdb 2023.09.01**
from https://mirrors.edge.kernel.org/pub/software/network/wireless-regdb/, unchanged.

Check the signature: `openssl smime -verify -binary -inform DER -in regulatory.db.p7s-sforshee -content regulatory.db-sforshee -CAfile sforshee.x509.pem -noverify -out /dev/null`
(sforshee.x509.pem is in the release tarball).
