from locust import HttpUser, task, between
import random

QUERIES = [
    "sertao", "cangaco", "quilombo", "amazonia", "favela",
    "ditadura", "resistencia", "periferia", "migração",
    "escravidao", "latifundio", "coronelismo", "folclore",
    "umbanda", "candomble", "desmatamento", "garimpo",
    "pantanal", "caatinga", "ribeirinho", "ancestralidade"
]

class UsuarioBusca(HttpUser):
    wait_time = between(0.01, 0.05)

    @task
    def buscar(self):
        query = random.choice(QUERIES)
        self.client.get(f"/search?q={query}")