# Dovecot for the integration tests: docker build -t rubi-dovecot testdata
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends dovecot-imapd ssl-cert && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 1000 vmail && useradd -u 1000 -g 1000 -d /srv/mail -m vmail
COPY dovecot.conf /etc/dovecot/dovecot.conf
EXPOSE 143
CMD ["dovecot", "-F"]
