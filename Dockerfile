# Copyright 
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


# Build the SDS server image

# Build the manager binary
FROM ubuntu:20.04 as builder

ARG SDK_VERSION="2.17.100.3"
ARG SGX_SDK_INSTALLER=sgx_linux_x64_sdk_${SDK_VERSION}.bin
ENV DEBIAN_FRONTEND=noninteractive
# SGX prerequisites
# hadolint ignore=DL3005,DL3008
RUN export HTTP_PROXY=http://child-prc.intel.com:913 \
  && export HTTPS_PROXY=http://child-prc.intel.com:913 \
  && export http_proxy=http://child-prc.intel.com:913 \
  && export https_proxy=http://child-prc.intel.com:913 \
  && apt-get update \
  && apt-get install --no-install-recommends -y \
    ca-certificates \
    curl \
    wget \
    unzip \
    protobuf-compiler \
    libprotobuf-dev \
    build-essential \
    patchelf \ 
    git \
    gnupg \
  && update-ca-certificates \ 
  # Add 01.org to apt for SGX packages
  # hadolint ignore=DL4006
  && echo "deb [arch=amd64] https://download.01.org/intel-sgx/sgx_repo/ubuntu focal main" >> /etc/apt/sources.list.d/intel-sgx.list \
  && wget -qO - https://download.01.org/intel-sgx/sgx_repo/ubuntu/intel-sgx-deb.key | apt-key add - \
  # Install SGX PSW
  && apt-get update \
  && apt-get install --no-install-recommends -y \
    libsgx-enclave-common=${SDK_VERSION}-focal1 \
    libsgx-launch=${SDK_VERSION}-focal1 \
    libsgx-launch-dev=${SDK_VERSION}-focal1 \
    libsgx-epid=${SDK_VERSION}-focal1 \
    libsgx-epid-dev=${SDK_VERSION}-focal1 \
    libsgx-quote-ex=${SDK_VERSION}-focal1 \
    libsgx-quote-ex-dev=${SDK_VERSION}-focal1 \
    libsgx-urts=${SDK_VERSION}-focal1 \
    libsgx-uae-service=${SDK_VERSION}-focal1 \
    libsgx-ae-epid=${SDK_VERSION}-focal1 \
    libsgx-ae-le=${SDK_VERSION}-focal1 \
    libsgx-ae-pce=${SDK_VERSION}-focal1 \
    libsgx-ae-qe3 \
    libsgx-ae-qve \
    libsgx-dcap-ql \
    libsgx-dcap-ql-dev \
    libsgx-pce-logic \
    libsgx-qe3-logic \
    libsgx-dcap-default-qpl \
  && apt-get clean \
  && ln -s /usr/lib/x86_64-linux-gnu/libsgx_enclave_common.so.1 /usr/lib/x86_64-linux-gnu/libsgx_enclave_common.so

# SGX SDK is installed in /opt/intel directory.
WORKDIR /opt/intel

# Install SGX SDK
# hadolint ignore=DL4006
RUN export HTTP_PROXY=http://child-prc.intel.com:913 \
  && export HTTPS_PROXY=http://child-prc.intel.com:913 \
  && export http_proxy=http://child-prc.intel.com:913 \
  && export https_proxy=http://child-prc.intel.com:913 \
  && wget https://download.01.org/intel-sgx/sgx-linux/2.17/distro/ubuntu20.04-server/$SGX_SDK_INSTALLER \
  && chmod +x  $SGX_SDK_INSTALLER \
  && echo "yes" | ./$SGX_SDK_INSTALLER \
  && rm $SGX_SDK_INSTALLER \
  && ls -l /opt/intel/

# Tag/commit-id/branch to use for bulding CTK
ARG CTK_TAG="master"

# Intel crypto-api-toolkit prerequisites
#https://github.com/intel/crypto-api-toolkit#software-requirements
RUN export HTTP_PROXY=http://child-prc.intel.com:913 \
  && export HTTPS_PROXY=http://child-prc.intel.com:913 \
  && export http_proxy=http://child-prc.intel.com:913 \
  && export https_proxy=http://child-prc.intel.com:913 \
  && set -x && apt-get update \
  && apt-get install --no-install-recommends -y \
    dkms libprotobuf17 autoconf \
    autotools-dev libc6-dev \
    libtool build-essential \
    opensc sudo \
    automake \
  && apt-get clean \
  && git clone https://github.com/intel/crypto-api-toolkit.git \
  && cd /opt/intel/crypto-api-toolkit \
  && git checkout ${CTK_TAG} -b v${CTK_TAG} \
  # disable building tests
  && sed -i -e 's;test;;g' ./src/Makefile.am \
  # disable enclave signing inside CTK
  #   && sed -i -e '/libp11SgxEnclave.signed.so/d' ./src/p11/trusted/Makefile.am \
  && ./autogen.sh \
  && ./configure --enable-dcap --with-token-path=/home/tcs-issuer \
  && make && make install

COPY LICENSE LICENSE
RUN mkdir -p /usr/local/share/package-licenses \
  && cp /opt/intel/crypto-api-toolkit/LICENSE.md /usr/local/share/package-licenses/crypto-api-toolkit.LICENSE \
  && ls -l /opt/intel/

###
# Clean runtime image which supposed to
# contain all runtime dependecy packages
###
FROM ubuntu:focal as runtime

ARG SDK_VERSION="2.17.100.3"
# ARG DCAP_VERSION="1.12.100.3"

RUN export HTTP_PROXY=http://child-prc.intel.com:913 \
  && export HTTPS_PROXY=http://child-prc.intel.com:913 \
  && export http_proxy=http://child-prc.intel.com:913 \
  && export https_proxy=http://child-prc.intel.com:913 \
  && apt-get update \
  && apt-get install -y wget gnupg \
  && echo "deb [arch=amd64] https://download.01.org/intel-sgx/sgx_repo/ubuntu focal main" >> /etc/apt/sources.list.d/intel-sgx.list \
  && wget -qO - https://download.01.org/intel-sgx/sgx_repo/ubuntu/intel-sgx-deb.key | apt-key add - \
  && sed -i '/deb-src/s/^# //' /etc/apt/sources.list \
  && apt-get update \
  && apt-get remove -y wget gnupg && apt-get autoremove -y \
  && bash -c 'set -o pipefail; apt-get install --no-install-recommends -y \
    libprotobuf17 \
    libsgx-enclave-common=${SDK_VERSION}-focal1 \
    libsgx-epid=${SDK_VERSION}-focal1 \
    libsgx-quote-ex=${SDK_VERSION}-focal1 \
    libsgx-urts=${SDK_VERSION}-focal1 \
    libsgx-ae-epid=${SDK_VERSION}-focal1 \
    libsgx-ae-qe3 \
    libsgx-dcap-ql \
    libsgx-pce-logic \
    libsgx-qe3-logic \
    libsgx-dcap-default-qpl \
    libsofthsm2 \
    # required for pkcs11-tool
    opensc | tee --append /usr/local/share/package-install.log' \
  && rm -rf /var/cache/* \
  && rm -rf  /var/log/*log /var/lib/apt/lists/* /var/log/apt/* /var/lib/dpkg/*-old /var/cache/debconf/*-old \
  && ln -s /usr/lib/x86_64-linux-gnu/libsgx_enclave_common.so.1 /usr/lib/x86_64-linux-gnu/libsgx_enclave_common.so

###
# Image that downloads the source packages for
#  the runtime GPL packages.
###

FROM ubuntu:focal as sources
COPY --from=runtime /usr/local/share/package-install.log /usr/local/share/package-install.log
COPY --from=runtime /usr/share/doc /tmp/runtime-doc
RUN export HTTP_PROXY=http://child-prc.intel.com:913 \
  && export HTTPS_PROXY=http://child-prc.intel.com:913 \
  && export http_proxy=http://child-prc.intel.com:913 \
  && export https_proxy=http://child-prc.intel.com:913 \
  && sed -i '/deb-src/s/^# //' /etc/apt/sources.list \
     && apt-get update \
     && mkdir /usr/local/share/package-sources && cd /usr/local/share/package-sources \ 
     && apt-get install -y git \
     && git clone https://github.com/hashicorp/go-multierror.git \
     && git clone https://github.com/hashicorp/go-version.git \
     && git clone https://github.com/hashicorp/golang-lru.git \
     && grep ^Get: /usr/local/share/package-install.log | grep -v sgx | cut -d ' ' -f 5,7 | \
         while read pkg version; do \
          if ! [ -f /tmp/runtime-doc/$pkg/copyright ]; then \
                    echo "ERROR: missing copyright file for $pkg"; \      
          fi; \
          if matches=$(grep -w -e MPL -e GPL -e LGPL /tmp/runtime-doc/$pkg/copyright); then \
             echo "INFO: downloading source of $pkg because of the following licenses:"; \
             echo "$matches" | sed -e 's/^/    /'; \
             apt-get source --download-only $pkg=$version || exit 1; \
          else \
             echo "INFO: not downloading source of $pkg, found no copyleft license"; \
          fi; \
         done \
     && apt-get clean

###
# Final sds-server Image
###
FROM runtime as final

RUN mkdir /sds
WORKDIR /sds

ADD sds-server /sds/sds-server

WORKDIR /

# RUN mkdir /usr/local/tmplibsgx

ADD copylib.sh copylib.sh
# RUN /bin/sh copylib.sh
ENV LD_LIBRARY_PATH="/usr/local/lib"
ENV SGX_LIBRARY_PATH="/usr/local/libsgx"
ENV SGX_TMP_LIBRARY_PATH="/usr/local/tmplibsgx"
RUN mkdir $SGX_TMP_LIBRARY_PATH

COPY --from=builder $LD_LIBRARY_PATH/libp11* $LD_LIBRARY_PATH/
COPY --from=builder /opt/intel /opt/intel
COPY --from=builder /usr/bin/patchelf /usr/bin/patchelf
COPY --from=builder $LD_LIBRARY_PATH/libp11SgxEnclave.signed.so $SGX_TMP_LIBRARY_PATH/libp11SgxEnclave.signed.so
COPY --from=builder $LD_LIBRARY_PATH/libp11sgx.so $SGX_TMP_LIBRARY_PATH/libp11sgx.so
COPY --from=builder /lib/x86_64-linux-gnu/libsgx_dcap_ql.so.1 $SGX_TMP_LIBRARY_PATH/libsgx_dcap_ql.so.1
COPY --from=builder /lib/x86_64-linux-gnu/libsgx_urts.so $SGX_TMP_LIBRARY_PATH/libsgx_urts.so
COPY --from=builder /lib/x86_64-linux-gnu/libsgx_qe3_logic.so $SGX_TMP_LIBRARY_PATH/libsgx_qe3_logic.so
COPY --from=builder /lib/x86_64-linux-gnu/libsgx_pce_logic.so.1 $SGX_TMP_LIBRARY_PATH/libsgx_pce_logic.so.1
COPY --from=builder /lib/x86_64-linux-gnu/libsgx_enclave_common.so.1 $SGX_TMP_LIBRARY_PATH/libsgx_enclave_common.so.1

# Copy licenses and sources
COPY --from=builder /usr/local/share/package-licenses /usr/local/share/package-licenses
COPY --from=sources /usr/local/share/package-sources /usr/local/share/package-sources

ENTRYPOINT ["/sds/sds-server"]