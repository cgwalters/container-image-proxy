# Some issue on f35 generating empty files list
%global debug_package %{nil}
%global goipath         github.com/cgwalters/container-image-proxy
%global gomodulesmode   GO111MODULE=on
Version:                0.1

%gometa

%global common_description %{expand:
See upstream README.md}

%global golicenses      LICENSE
%global godocs          README.md

Name:           container-image-proxy
Release:        1%{?dist}
Summary:        container image proxy
License:        ASL 2.0
URL:            %{gourl}
Source0:        %{gosource}

%description
%{common_description}

%prep
%autosetup
%goprep -k
%autopatch -p1

%build
%set_build_flags
%make_build

%install
%make_install

%files
%license %{golicenses}
%{_bindir}/container-image-proxy
