<?php
declare(strict_types=1);

namespace App\Form;

use Symfony\Component\Form\AbstractType;
use Symfony\Component\Form\Extension\Core\Type\{PasswordType, TextType};
use Symfony\Component\Form\FormBuilderInterface;

class LoginType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options): void
    {
        $builder
            ->add('_username', TextType::class, ['mapped' => false, 'required' => true])
            ->add('_password', PasswordType::class, ['mapped' => false, 'required' => true])
        ;
    }

    public function getBlockPrefix(): string
    {
        // Empty prefix for default UsernamePasswordFormAuthenticationListener
        return '';
    }
}
