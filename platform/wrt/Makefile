include $(TOPDIR)/rules.mk

PKG_NAME:=marten-core
PKG_VERSION:=3.1.8
PKG_RELEASE:=1

PKG_SOURCE:=$(PKG_NAME)-$(PKG_VERSION).tar.gz
PKG_SOURCE_URL:=https://codeload.github.com/Cr0me1ve/marten-core/tar.gz/v$(PKG_VERSION)?
PKG_HASH:=02c8db701ed3b6fc7e5ccc91d444edb6fa3ca8faa143cd2a2678f4f154eafd8d

PKG_LICENSE:=https://github.com/Cr0me1ve/Marten/blob/main/LICENSE.md
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=Marten <wrt@marten.app>

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/Cr0me1ve/marten-core
GO_PKG_BUILD_PKG:=$(GO_PKG)/cli

GO_PKG_LDFLAGS_X:=$(GO_PKG)/constant.Version=$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk
include ../../lang/golang/golang-package.mk

define Package/marten-core
  TITLE:=Marten universal proxy platform
  SECTION:=net
  CATEGORY:=Network
  URL:=https://marten.app/
  DEPENDS:=$(GO_ARCH_DEPENDS) +ca-bundle +kmod-inet-diag +kmod-tun
  USERID:=marten=15566:marten=15566
endef

define Package/marten-core/description
  Marten is a universal proxy platform which supports hysteria, SOCKS, Shadowsocks,
  ShadowTLS, Tor, trojan, VLess, VMess, WireGuard and so on.
endef

GO_PKG_TAGS:=with_gvisor,with_quic,with_wireguard,with_ech,with_utls,with_clash_api,with_grpc,with_low_memory,with_conntrack

define Package/marten-core/conffiles
/etc/config/marten
/etc/marten/
endef

define Package/marten-core/install

	$(INSTALL_DIR) $(1)/usr/bin/
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/cli $(1)/usr/bin/marten-cli

	$(INSTALL_DIR) $(1)/etc/marten
	$(INSTALL_DATA) ./files/marten.json $(1)/etc/marten/appconfig.json

	$(INSTALL_DIR) $(1)/etc/config/
	$(INSTALL_CONF) ./files/marten.conf $(1)/etc/config/marten

	$(INSTALL_DIR) $(1)/etc/init.d/
	$(INSTALL_BIN) ./files/marten.init $(1)/etc/init.d/marten

	$(CP) ./files/webui $(1)/usr/bin/
endef

$(eval $(call BuildPackage,marten-core))
