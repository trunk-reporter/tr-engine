###############################################################################
# Stage 1 – compile trunk-recorder + SoapyPlutoPAPR (builder image)           #
###############################################################################
FROM ubuntu:noble-20251001 AS builder

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -y upgrade && \
    apt-get install --no-install-recommends -y \
        build-essential cmake git curl pkg-config wget \
        ffmpeg gnuradio-dev gr-osmosdr libosmosdr-dev \
        libairspy-dev libairspyhf-dev libbladerf-dev libfreesrp-dev \
        libhackrf-dev libmirisdr-dev libuhd-dev libxtrx-dev librtlsdr-dev \
        libsoapysdr-dev \
        libboost-all-dev libcurl4-openssl-dev libgmp-dev liborc-0.4-dev \
        libpaho-mqtt-dev libpaho-mqttpp-dev libpthread-stubs0-dev libsndfile1-dev \
        libssl-dev python3-six openssh-client ca-certificates \
        libiio-dev libad9361-dev libserialport-dev flex bison libxml2-dev soapysdr-tools \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# ───────────────────────── trunk-recorder (core) ─────────────────────────────
RUN git clone --depth 1 https://github.com/TrunkRecorder/trunk-recorder.git && \
    sed -i 's/^[[:space:]]*#\s*add_subdirectory(plugins\/simplestream)/add_subdirectory(plugins\/simplestream)/' \
        trunk-recorder/CMakeLists.txt && \
    mkdir -p trunk-recorder/build

# ── Patch 1: simplestream sendJSON dangling pointer fix (PR #1107) ──────────
COPY simplestream_fix.py /tmp/
RUN cd trunk-recorder && python3 /tmp/simplestream_fix.py plugins/simplestream/simplestream.cc

# ── Patch 2: conventional call_start on squelch open ────────────────────────
COPY call_start_fix.py /tmp/
RUN python3 /tmp/call_start_fix.py trunk-recorder/trunk-recorder/monitor_systems.cc

# ── Patch 3: IMBE symbol tap ────────────────────────────────────────────────
COPY imbe_tap.h imbe_tap.cc /src/trunk-recorder/lib/op25_repeater/lib/
COPY imbe_tap_patch.py /tmp/
RUN cd trunk-recorder && python3 /tmp/imbe_tap_patch.py

# ── MQTT plugin (upstream has reconnect fix merged) ─────────────────────────
RUN git -C trunk-recorder/user_plugins \
       clone --depth 1 https://github.com/TrunkRecorder/tr-plugin-mqtt.git

WORKDIR /src/trunk-recorder/build
RUN cmake .. \
 && make -j"$(nproc)" \
 && make DESTDIR=/newroot install

# ───────────────────────── SoapyPlutoPAPR driver ────────────────────────────
RUN cd /tmp && \
    git clone --depth 1 https://github.com/F5OEO/SoapyPlutoPAPR.git && \
    cmake -S SoapyPlutoPAPR -B SoapyPlutoPAPR/build \
          -DCMAKE_INSTALL_PREFIX=/usr \
          -DCMAKE_INSTALL_LIBDIR=lib/x86_64-linux-gnu && \
    cmake --build SoapyPlutoPAPR/build -- -j"$(nproc)" && \
    DESTDIR=/newroot cmake --install SoapyPlutoPAPR/build && \
    rm -rf SoapyPlutoPAPR

###############################################################################
# Stage 2 – lightweight runtime image                                         #
###############################################################################
FROM ubuntu:noble-20251001

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -y upgrade && \
    apt-get install --no-install-recommends -y \
        ca-certificates curl wget sox fdkaac docker.io \
        libboost-chrono1.83.0t64 libboost-log1.83.0 \
        libgnuradio-analog3.10.9t64 libgnuradio-digital3.10.9t64 \
        libgnuradio-filter3.10.9t64 libgnuradio-network3.10.9t64 \
        libgnuradio-osmosdr0.2.0t64 libgnuradio-uhd3.10.9t64 \
        libpaho-mqtt-dev libpaho-mqttpp-dev \
        libairspyhf1 libfreesrp0 librtlsdr2 libxtrx0 \
        libsoapysdr0.8 \
        libiio0 libad9361-0 \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/share/{doc,man,info} /usr/local/share/{doc,man,info}

COPY --from=builder /newroot /

RUN mkdir -p /etc/gnuradio/conf.d && \
    echo 'log_level = info' > /etc/gnuradio/conf.d/gnuradio-runtime.conf && \
    ldconfig

WORKDIR /app
ENV HOME=/tmp

ENTRYPOINT ["trunk-recorder", "--config=/app/config.json"]
