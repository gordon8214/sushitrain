# Synctrain for Linux (Docs/linux.md). Linux/build/build-rpm.sh builds the programs first and runs rpmbuild on this:
# the daemons need the Go release go.mod names, newer than Fedora's, and the window GTK's development packages.
#
# The names Sushitrain and Synctrain and the final logo are not under the MPL (README.md, License): this package is
# for the maintainer's own machines. Publishing it, as in a public COPR, needs their owner's permission.
Name:           synctrain
Version:        %{synctrain_version}
Release:        %{synctrain_release}%{?dist}
Summary:        Synchronize folders with Syncthing, or open a gateway's folders on demand
License:        MPL-2.0
URL:            https://github.com/gordon8214/sushitrain
ExclusiveArch:  x86_64 aarch64

BuildRequires:  systemd-rpm-macros
Requires:       fuse3
Requires:       gtk4
Requires:       libadwaita >= 1.9
Requires:       poppler-utils
Requires:       gstreamer1-plugins-base
Recommends:     gstreamer1-plugins-good
# Video and PDF thumbnails for clients of the remote file service
Recommends:     ffmpeg-free
Recommends:     poppler-utils

# The programs are built and stripped already
%global debug_package %{nil}

%description
Synctrain runs in one of two modes on each machine. In Synchronize mode, Syncthing runs here, and phones and other
machines can reach its folders through Synctrain's remote file service. In Files mode, no Syncthing runs here: the
folders of a gateway on another system appear in ~/Synctrain, and files are fetched as they are opened.

%install
install -Dm755 %{_sourcedir}/synctrain %{buildroot}%{_bindir}/synctrain
install -Dm755 %{_sourcedir}/synctrain-files %{buildroot}%{_bindir}/synctrain-files
install -Dm755 %{_sourcedir}/synctrain-gateway %{buildroot}%{_bindir}/synctrain-gateway
install -Dm644 %{_sourcedir}/synctrain-files.service %{buildroot}%{_userunitdir}/synctrain-files.service
install -Dm644 %{_sourcedir}/synctrain-gateway.service %{buildroot}%{_userunitdir}/synctrain-gateway.service
install -Dm644 %{_sourcedir}/com.gordonbyrnes.Synctrain.desktop \
	%{buildroot}%{_datadir}/applications/com.gordonbyrnes.Synctrain.desktop
install -Dm644 %{_sourcedir}/com.gordonbyrnes.Synctrain.metainfo.xml \
	%{buildroot}%{_metainfodir}/com.gordonbyrnes.Synctrain.metainfo.xml
install -Dm644 %{_sourcedir}/com.gordonbyrnes.Synctrain.png \
	%{buildroot}%{_datadir}/icons/hicolor/256x256/apps/com.gordonbyrnes.Synctrain.png
install -Dm755 %{_sourcedir}/libsynctrain-nautilus.so %{buildroot}%{_libdir}/nautilus/extensions-4/libsynctrain-nautilus.so
install -Dm644 %{_sourcedir}/LICENSE %{buildroot}%{_licensedir}/%{name}/LICENSE
install -Dm644 %{_sourcedir}/linux.md %{buildroot}%{_docdir}/%{name}/linux.md

# The window enables one of the units when a mode is chosen; nothing runs before that
%post
%systemd_user_post synctrain-files.service synctrain-gateway.service

%preun
%systemd_user_preun synctrain-files.service synctrain-gateway.service

# Not restarted on an upgrade: restarting synctrain-files unmounts ~/Synctrain under open files. The new version runs
# from the next login, or when the window starts it again.
%postun
%systemd_user_postun synctrain-files.service synctrain-gateway.service

%files
%dir %{_licensedir}/%{name}
%license %{_licensedir}/%{name}/LICENSE
%dir %{_docdir}/%{name}
%doc %{_docdir}/%{name}/linux.md
%{_bindir}/synctrain
%{_bindir}/synctrain-files
%{_bindir}/synctrain-gateway
%{_userunitdir}/synctrain-files.service
%{_userunitdir}/synctrain-gateway.service
%{_datadir}/applications/com.gordonbyrnes.Synctrain.desktop
%{_metainfodir}/com.gordonbyrnes.Synctrain.metainfo.xml
%{_datadir}/icons/hicolor/256x256/apps/com.gordonbyrnes.Synctrain.png
%{_libdir}/nautilus/extensions-4/libsynctrain-nautilus.so
