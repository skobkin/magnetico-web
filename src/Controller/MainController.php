<?php

namespace App\Controller;

use App\Form\LoginType;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\Form\Extension\Core\Type\SubmitType;
use Symfony\Component\Form\FormInterface;
use Symfony\Component\HttpFoundation\Response;

class MainController extends AbstractController
{
    public function index(): Response
    {
        return $this->render('index.html.twig', [
            'loginForm' => $this->createLoginForm('')->createView(),
        ]);
    }

    private function createLoginForm(string $username): FormInterface
    {
        $form = $this->createForm(LoginType::class, null, [
            'action' => $this->generateUrl('user_auth_login'),
        ]);
        $form->get('_username')->setData($username);
        $form->add('submit', SubmitType::class);

        return $form;
    }
}