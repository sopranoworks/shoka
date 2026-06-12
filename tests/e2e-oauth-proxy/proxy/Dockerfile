# Apache httpd reverse proxy + TLS terminator for the B-61 OAuth e2e harness.
# The directive mandates Apache httpd (mod_proxy/mod_proxy_http/mod_ssl); no
# other proxy is permitted.
FROM httpd:2.4

# Our vhost config supplies its own LoadModule lines, so just Include it from the
# stock httpd.conf.
RUN echo "Include conf/extra/shoka-proxy.conf" >> /usr/local/apache2/conf/httpd.conf

COPY proxy/shoka-proxy.conf /usr/local/apache2/conf/extra/shoka-proxy.conf

# TLS material (local test CA leaf) and the client's CIMD document are mounted /
# copied in by docker-compose build context.
COPY certs/server.crt /usr/local/apache2/conf/tls/server.crt
COPY certs/server.key /usr/local/apache2/conf/tls/server.key
COPY client/cimd/client.json /usr/local/apache2/htdocs/client/cimd/client.json

EXPOSE 443
